package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/audio"
	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/minutes"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/search"
	"github.com/ZaneXo/zsmeet/internal/storage"
	"github.com/ZaneXo/zsmeet/internal/transcript"
)

const (
	// 分片序号上限。网页录音每 10 秒一片（录满 6 小时是 2160 片），上传文件每片 4 MB
	MaxChunks = 4000
	// 一场录音最多标记多少个重点
	MaxMarks = 500
	// 单场会议上传的总大小上限
	maxUploadBytes = storage.MaxRecordingBytes
	maxTitleChars  = 200
)

// Meeting 是会议加上创建者的名字，和取出它的人对它的权限。
type Meeting struct {
	sqlc.Meeting
	OwnerName string
	// 搜索时命中的句子（转写）和行（纪要），不是搜索时为空
	Hits []search.Hit
	// 取出这场会议的人能对它做什么（见 Access）
	Access Access
}

type NewMeeting struct {
	Title    string
	Source   string
	Template string
	Hotwords string
	// 谁能看：team（默认）或者 private。指定具体的人在建好以后的会议页面上做
	Visibility string
}

// CreateMeeting 新建一场等待上传的会议。
func (s *Service) CreateMeeting(ctx context.Context, user sqlc.User, in NewMeeting) (Meeting, error) {
	title, err := cleanTitle(in.Title)
	if err != nil {
		return Meeting{}, err
	}
	if title == "" {
		title = time.Now().Format("1月2日 15:04") + " 的会议"
	}
	if in.Source != "upload" && in.Source != "record" {
		return Meeting{}, invalid("不认识的会议来源：%s", in.Source)
	}
	if in.Template == "" {
		in.Template = minutes.Templates[0].ID
	}
	if err := s.checkTemplate(ctx, in.Template); err != nil {
		return Meeting{}, err
	}
	hotwords := strings.TrimSpace(in.Hotwords)
	if utf8.RuneCountInString(hotwords) > 500 {
		return Meeting{}, invalid("热词太多了，最长 500 个字")
	}
	switch in.Visibility {
	case "":
		in.Visibility = VisibilityTeam
	case VisibilityTeam, VisibilityPrivate:
	default:
		return Meeting{}, invalid("新建会议时可见范围只能是团队所有人或者只有自己，指定具体的人请建好以后在会议页面上设置")
	}

	var m sqlc.Meeting
	err = s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		var err error
		m, err = q.CreateMeeting(ctx, sqlc.CreateMeetingParams{
			OwnerID: user.ID, Title: title, Source: in.Source, Template: in.Template, Hotwords: hotwords, Visibility: in.Visibility,
		})
		if err != nil {
			return err
		}
		return search.Reindex(ctx, q, m.ID)
	})
	return Meeting{Meeting: m, OwnerName: user.Name, Access: fullAccess}, err
}

func cleanTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if utf8.RuneCountInString(title) > maxTitleChars {
		return "", invalid("标题最长 %d 个字", maxTitleChars)
	}
	return title, nil
}

// 搜索时每次从索引取多少个候选会议。索引是按「两字词」粗筛的，会有误命中，要逐句找过才知道是不是真的搜到了
const searchPage = 100

// 「错一个字」的近似匹配最多检查多少个候选会议。这种候选误命中多，全查太慢；没查完时告诉用户
const maxFuzzyScan = 500

// 一次搜索最多用几种写法（用户输入的加上术语表里的）
const maxSpellings = 10

// MeetingList 是会议列表，搜索时还带着实际搜了哪几种写法。
type MeetingList struct {
	Items []Meeting
	// 搜索时实际搜了哪几种写法：用户输入的，加上各场会议术语表里和它对应的写法。不是搜索时为空
	Spellings []string
	// 「错一个字」的近似匹配没查完所有候选会议，只查了最近的一部分
	Partial bool
}

// ListMeetings 按时间倒序列出 viewer 能看的会议。owner 不为 nil 时只列这个人创建的。
// query 不为空时在标题、纪要和转写里搜索，每场会议带上命中的句子（Meeting.Hits）：
// 先列真的搜到的会议，不够 limit 场时再列只是错了一个字的，都按时间倒序。
func (s *Service) ListMeetings(ctx context.Context, viewer sqlc.User, query string, owner *uuid.UUID, limit int) (MeetingList, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		rows, err := s.q.ListMeetings(ctx, sqlc.ListMeetingsParams{Viewer: viewer.ID, IsAdmin: viewer.Role == "admin", Owner: owner, MaxRows: int32(limit)})
		if err != nil {
			return MeetingList{}, err
		}
		out := make([]Meeting, len(rows))
		for i, r := range rows {
			out[i] = listed(viewer, r)
		}
		return MeetingList{Items: out}, nil
	}

	return s.searchMeetings(ctx, viewer, query, owner, limit, period{})
}

// period 是按会议时间筛选的范围：from 含、to 不含，nil 表示不限。
type period struct {
	from, to *time.Time
}

// searchMeetings 在 viewer 能看的、时间在 within 里的会议中搜索 query，见 ListMeetings。
func (s *Service) searchMeetings(ctx context.Context, viewer sqlc.User, query string, owner *uuid.UUID, limit int, within period) (MeetingList, error) {
	spellings, err := s.spellings(ctx, viewer, query)
	if err != nil {
		return MeetingList{}, err
	}
	list := MeetingList{Items: []Meeting{}, Spellings: spellings}
	sc := &searchScan{s: s, viewer: viewer, owner: owner, within: within, spellings: spellings, limit: limit, list: &list, seen: map[uuid.UUID]bool{}}
	// 搜的全是标点符号之类切不出词的东西时什么都匹配不上，tsquery 是空的
	if exact := search.TSQuery(spellings); exact != "" {
		if _, err := sc.run(ctx, exact, 0, func(r search.Result) bool { return r.Matched }); err != nil {
			return MeetingList{}, err
		}
	}
	if fuzzy := search.FuzzyTSQuery(query); fuzzy != "" && len(list.Items) < limit {
		list.Partial, err = sc.run(ctx, fuzzy, maxFuzzyScan, func(r search.Result) bool { return r.Fuzzy })
		if err != nil {
			return MeetingList{}, err
		}
	}
	return list, nil
}

// listed 把列表里的一行变成 Meeting，带上 viewer 的权限。
func listed(viewer sqlc.User, r sqlc.ListMeetingsRow) Meeting {
	return Meeting{Meeting: r.Meeting, OwnerName: r.OwnerName, Access: accessFor(viewer, r.Meeting, r.IsMember, r.MemberCanEdit)}
}

// searchScan 是一次搜索的进度：按时间从新到旧一页一页取索引粗筛出的候选会议，逐句找命中，留下的放进 list。
type searchScan struct {
	s         *Service
	viewer    sqlc.User
	owner     *uuid.UUID
	within    period
	spellings []string
	limit     int
	list      *MeetingList
	// 已经检查过的会议，第二轮（错一个字）不再重复检查
	seen map[uuid.UUID]bool
}

// run 用 tsq 粗筛候选，keep 返回 true 的留下，直到凑够 limit 场或者候选取完。
// budget 大于 0 时最多检查这么多个候选；返回值表示是不是因为它停下的（还有没检查的候选）。
func (sc *searchScan) run(ctx context.Context, tsq string, budget int, keep func(search.Result) bool) (bool, error) {
	params := sqlc.ListMeetingsParams{
		Viewer: sc.viewer.ID, IsAdmin: sc.viewer.Role == "admin", Query: &tsq, Owner: sc.owner, MaxRows: searchPage,
	}
	if sc.within.to != nil {
		// (created_at, id) < (to, 全零的 id)：正好是 created_at < to
		params.BeforeAt, params.BeforeID = sc.within.to, &uuid.Nil
	}
	checked := 0
	for len(sc.list.Items) < sc.limit {
		page, err := sc.s.q.ListMeetings(ctx, params)
		if err != nil {
			return false, err
		}
		// 按时间倒序取的，早于 from 的会议后面都是，不用再往下翻
		rows := page
		if sc.within.from != nil {
			if i := slices.IndexFunc(page, func(r sqlc.ListMeetingsRow) bool { return r.Meeting.CreatedAt.Before(*sc.within.from) }); i >= 0 {
				rows = page[:i]
			}
		}
		var ids []uuid.UUID
		for _, r := range rows {
			if !sc.seen[r.Meeting.ID] {
				ids = append(ids, r.Meeting.ID)
			}
		}
		segs, minutesOf, err := sc.s.searchTexts(ctx, ids)
		if err != nil {
			return false, err
		}
		for _, r := range rows {
			id := r.Meeting.ID
			if sc.seen[id] {
				continue
			}
			sc.seen[id] = true
			checked++
			res := search.Find(r.Meeting.Title, minutesOf[id], segs[id], sc.spellings)
			if !keep(res) {
				continue
			}
			m := listed(sc.viewer, r)
			m.Hits = res.Hits
			sc.list.Items = append(sc.list.Items, m)
			if len(sc.list.Items) >= sc.limit {
				return false, nil
			}
		}
		if len(page) < searchPage || len(rows) < len(page) {
			return false, nil
		}
		if budget > 0 && checked >= budget {
			return true, nil
		}
		last := rows[len(rows)-1].Meeting
		params.BeforeAt, params.BeforeID = &last.CreatedAt, &last.ID
	}
	return false, nil
}

// searchTexts 取一批会议的转写（按句）和当前版本的纪要，用来逐句找命中。
func (s *Service) searchTexts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]transcript.Segment, map[uuid.UUID]string, error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	segs, err := s.transcriptsFor(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.q.CurrentMinutesFor(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	minutesOf := make(map[uuid.UUID]string, len(rows))
	for _, r := range rows {
		minutesOf[r.MeetingID] = r.Content
	}
	return segs, minutesOf, nil
}

// spellings 返回要搜的几种写法：用户输入的排第一，后面是各场会议术语表里和它对应的写法
// （它就是某个词的正确写法或者听错写法时，这个词的所有写法都加进来）。
// 比较时不分大小写、不管空格，和 TermsMatching 的口径一样。太短、没有字的写法不要（见 minutes.UsefulTerm）；
// 包含了前面某种写法的也不要（「这识别模型」）：搜前面那种写法时本来就能找到它。
func (s *Service) spellings(ctx context.Context, viewer sqlc.User, query string) ([]string, error) {
	out := []string{query}
	keys := []string{spellingKey(query)}
	add := func(sp string) {
		sp = strings.TrimSpace(sp)
		key := spellingKey(sp)
		if !minutes.UsefulTerm(sp) || len(out) >= maxSpellings || slices.ContainsFunc(keys, func(k string) bool { return strings.Contains(key, k) }) {
			return
		}
		keys = append(keys, key)
		out = append(out, sp)
	}
	rows, err := s.q.TermsMatching(ctx, sqlc.TermsMatchingParams{Viewer: viewer.ID, IsAdmin: viewer.Role == "admin", Q: query})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		add(r.Term)
		var variants []string
		if json.Unmarshal(r.Variants, &variants) == nil {
			for _, v := range variants {
				add(v)
			}
		}
	}
	return out, nil
}

// spellingKey 是比较两种写法是不是同一个时用的：转小写、去掉所有空白。
func spellingKey(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), "")
}

// transcriptsFor 取一批会议的转写（按句）。
func (s *Service) transcriptsFor(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]transcript.Segment, error) {
	rows, err := s.q.TranscriptsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID][]transcript.Segment, len(rows))
	for _, r := range rows {
		var segs []transcript.Segment
		if err := json.Unmarshal(r.Segments, &segs); err != nil {
			return nil, err
		}
		out[r.MeetingID] = segs
	}
	return out, nil
}

var errNoMeeting = notFound("会议不存在，可能已被删除")

// getMeeting 取出一场会议，不检查权限（后台和权限检查本身用）。回收站里的会议当作不存在。
func (s *Service) getMeeting(ctx context.Context, id uuid.UUID) (Meeting, error) {
	row, err := s.q.GetMeeting(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.Meeting.DeletedAt != nil) {
		// 放进回收站的会议当作不存在，回收站另有自己的接口
		return Meeting{}, errNoMeeting
	}
	return Meeting{Meeting: row.Meeting, OwnerName: row.OwnerName}, err
}

func (s *Service) RenameMeeting(ctx context.Context, user sqlc.User, id uuid.UUID, title string) (Meeting, error) {
	title, err := cleanTitle(title)
	if err != nil {
		return Meeting{}, err
	}
	if title == "" {
		return Meeting{}, invalid("标题不能为空")
	}
	if _, err := s.editable(ctx, user, id); err != nil {
		return Meeting{}, err
	}
	err = s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		if _, err := q.RenameMeeting(ctx, sqlc.RenameMeetingParams{ID: id, Title: title}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errNoMeeting
			}
			return err
		}
		return search.Reindex(ctx, q, id)
	})
	if err != nil {
		return Meeting{}, err
	}
	return s.GetMeeting(ctx, user, id)
}

// PutChunk 保存一个上传分片。同一片可以重复上传（断点续传时会发生），后一次覆盖前一次。
func (s *Service) PutChunk(ctx context.Context, user sqlc.User, id uuid.UUID, seq int, body io.Reader) error {
	if seq < 0 || seq >= MaxChunks {
		return invalid("分片序号超出范围")
	}
	m, err := s.manageable(ctx, user, id)
	if err != nil {
		return err
	}
	if m.Status != "uploading" {
		return conflict("这场会议已经上传完成，不能再传分片")
	}
	size, err := s.store.PutChunk(id, seq, body)
	if err != nil {
		return err
	}
	if size == 0 {
		return invalid("分片是空的")
	}
	if err := s.q.AddChunk(ctx, sqlc.AddChunkParams{MeetingID: id, Seq: int32(seq), Size: size}); err != nil {
		return err
	}
	// 记下最后收到分片的时间，网页据此显示录音还在不在进行
	return s.q.TouchChunkTime(ctx, id)
}

// ReceivedChunks 返回已经收到的分片序号，网页断点续传时用它跳过传过的。
func (s *Service) ReceivedChunks(ctx context.Context, user sqlc.User, id uuid.UUID) ([]int, error) {
	if _, err := s.GetMeeting(ctx, user, id); err != nil {
		return nil, err
	}
	rows, err := s.q.ListChunks(ctx, id)
	if err != nil {
		return nil, err
	}
	seqs := make([]int, len(rows))
	for i, r := range rows {
		seqs[i] = int(r.Seq)
	}
	return seqs, nil
}

// CompleteUpload 在所有分片传完后调用：确认 0 到 total-1 都收到了，然后交给后台处理。
// marks 是录音时标记的重点（录音开始后的秒数），没有就传空。
func (s *Service) CompleteUpload(ctx context.Context, user sqlc.User, id uuid.UUID, total int, marks []float64) (Meeting, error) {
	if total < 1 || total > MaxChunks {
		return Meeting{}, invalid("分片数量不对")
	}
	cleaned, err := cleanMarks(marks)
	if err != nil {
		return Meeting{}, err
	}
	if _, err := s.manageable(ctx, user, id); err != nil {
		return Meeting{}, err
	}
	rows, err := s.q.ListChunks(ctx, id)
	if err != nil {
		return Meeting{}, err
	}
	var bytes int64
	for i := range total {
		if i >= len(rows) || int(rows[i].Seq) != i {
			return Meeting{}, conflict("第 %d 片还没有传上来，请补传后再试", i+1)
		}
		bytes += rows[i].Size
	}
	if len(rows) > total {
		return Meeting{}, conflict("收到的分片比声明的多，请重新上传")
	}
	if bytes > maxUploadBytes {
		return Meeting{}, invalid("文件超过 %d GB，请压缩或拆分后再上传", maxUploadBytes>>30)
	}
	// 没带标记时保留录音过程中已经存下的（从会议页面收尾时就不带）。
	// 会议已经不在上传状态时这里什么也不改，下面入队会报「已经在处理了」
	if len(cleaned) > 0 {
		if _, err := s.q.SetMeetingMarks(ctx, sqlc.SetMeetingMarksParams{ID: id, Marks: cleaned}); err != nil {
			return Meeting{}, err
		}
	}
	return s.enqueue(ctx, user, id, []string{"uploading"}, nil, "这场会议已经在处理了")
}

// SaveMarks 在录音过程中保存标记的重点（整份替换）。录音的网页每标记一次就传一次，
// 这样录音的电脑找不回来、在会议页面上用已经收到的部分生成纪要时，标记也还在。
func (s *Service) SaveMarks(ctx context.Context, user sqlc.User, id uuid.UUID, marks []float64) error {
	cleaned, err := cleanMarks(marks)
	if err != nil {
		return err
	}
	if _, err := s.manageable(ctx, user, id); err != nil {
		return err
	}
	n, err := s.q.SetMeetingMarks(ctx, sqlc.SetMeetingMarksParams{ID: id, Marks: cleaned})
	if err != nil {
		return err
	}
	if n == 0 {
		return conflict("这场会议已经结束录音、开始处理了，不能再改标记")
	}
	return nil
}

// cleanMarks 检查录音时标记的重点：数量有上限，时间不能是负数或者超过录音时长上限。
// 结果按时间排序，去掉重复的（同一秒里点了好几下）。
func cleanMarks(marks []float64) ([]float32, error) {
	if len(marks) > MaxMarks {
		return nil, invalid("标记的重点太多了，最多 %d 个", MaxMarks)
	}
	out := make([]float32, 0, len(marks))
	for _, t := range marks {
		if math.IsNaN(t) || t < 0 || t > audio.MaxSeconds {
			return nil, invalid("标记重点的时间不对：%v", t)
		}
		out = append(out, float32(math.Round(t*10)/10))
	}
	slices.Sort(out)
	kept := out[:0]
	for _, t := range out {
		// 和上一个隔不到 1 秒的算同一个：多半是连着点了几下
		if len(kept) > 0 && t-kept[len(kept)-1] < 1 {
			continue
		}
		kept = append(kept, t)
	}
	return kept, nil
}

// Retry 重新处理一场失败的会议。已经有转写的话只重写纪要。
func (s *Service) Retry(ctx context.Context, user sqlc.User, id uuid.UUID) (Meeting, error) {
	m, err := s.editable(ctx, user, id)
	if err != nil {
		return Meeting{}, err
	}
	// 录音过了保留期被删了、又还没有转写的话，重试也只能失败（有转写的照常重试，只重写纪要）
	if m.AudioDeletedAt != nil {
		if _, err := s.q.GetTranscript(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return Meeting{}, conflict("这场会议的录音已经过了保留期被删除，又还没有转写出来，没法重新处理")
		} else if err != nil {
			return Meeting{}, err
		}
	}
	return s.enqueue(ctx, user, id, []string{"failed"}, nil, "只有处理失败的会议才能重试")
}

var errAudioGone = conflict("这场会议的录音已经不在了（可能过了保留期），没法重新转写")

// Retranscribe 用现在的识别模型重新转写一场会议（换了识别模型，或者转写有问题时用），
// 转完会重新生成纪要，作为一个新版本，之前的版本都保留。
func (s *Service) Retranscribe(ctx context.Context, user sqlc.User, id uuid.UUID) (Meeting, error) {
	m, err := s.editable(ctx, user, id)
	if err != nil {
		return Meeting{}, err
	}
	if m.AudioKey == "" || m.AudioDeletedAt != nil || !s.store.Exists(m.AudioKey) {
		return Meeting{}, errAudioGone
	}
	runID := uuid.New()
	err = s.tx(ctx, func(tx pgx.Tx, q *sqlc.Queries) error {
		queued, err := q.QueueMeeting(ctx, sqlc.QueueMeetingParams{
			ID: id, RunID: &runID, RunBy: &user.ID, FromStatus: []string{"done", "failed"},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return conflict("会议正在处理中，等处理完再重新转写")
		}
		if err != nil {
			return err
		}
		// 上面检查完以后，巡检可能刚好把过了保留期的录音删了（它只删已完成、失败的会议，排上队以后就不会再删）
		if queued.AudioDeletedAt != nil {
			return errAudioGone
		}
		// 后台看到这个标记就从存好的录音重新识别。旧的转写先留着：重新识别失败时转写和纪要都还在
		if err := q.MarkRetranscribe(ctx, id); err != nil {
			return err
		}
		_, err = s.queue.InsertTx(ctx, tx, queue.ProcessArgs{MeetingID: id, RunID: runID}, nil)
		return err
	})
	if err != nil {
		return Meeting{}, err
	}
	return s.GetMeeting(ctx, user, id)
}

// Regenerate 用指定模板重新生成纪要，结果是一个新版本，旧版本保留。
func (s *Service) Regenerate(ctx context.Context, user sqlc.User, id uuid.UUID, template string) (Meeting, error) {
	if err := s.checkTemplate(ctx, template); err != nil {
		return Meeting{}, err
	}
	if _, err := s.editable(ctx, user, id); err != nil {
		return Meeting{}, err
	}
	cfg, err := s.settings.LLM(ctx)
	if err != nil {
		return Meeting{}, err
	}
	if !cfg.Ready() {
		return Meeting{}, conflict("还没有配置大模型，请管理员先到「设置」里填写")
	}
	if _, err := s.q.GetTranscript(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		if _, err := s.getMeeting(ctx, id); err != nil {
			return Meeting{}, err
		}
		return Meeting{}, conflict("这场会议还没有转写，不能生成纪要")
	} else if err != nil {
		return Meeting{}, err
	}
	return s.enqueue(ctx, user, id, []string{"done"}, &template, "会议正在处理中，等处理完再重新生成")
}

// enqueue 把会议改成排队状态并提交后台任务，两件事在同一个事务里。
// 会议当前状态不在 from 里时返回 Conflict，文案是 busy。
func (s *Service) enqueue(ctx context.Context, user sqlc.User, id uuid.UUID, from []string, template *string, busy string) (Meeting, error) {
	runID := uuid.New()
	err := s.tx(ctx, func(tx pgx.Tx, q *sqlc.Queries) error {
		_, err := q.QueueMeeting(ctx, sqlc.QueueMeetingParams{
			ID: id, RunID: &runID, RunBy: &user.ID, Template: template, FromStatus: from,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := q.GetMeeting(ctx, id); errors.Is(err, pgx.ErrNoRows) {
				return errNoMeeting
			}
			return conflict("%s", busy)
		}
		if err != nil {
			return err
		}
		_, err = s.queue.InsertTx(ctx, tx, queue.ProcessArgs{MeetingID: id, RunID: runID}, nil)
		return err
	})
	if err != nil {
		return Meeting{}, err
	}
	return s.GetMeeting(ctx, user, id)
}

// Transcript 返回转写。还没有转写时返回空列表。
func (s *Service) Transcript(ctx context.Context, user sqlc.User, id uuid.UUID) ([]transcript.Segment, error) {
	// 先确认会议在（没被删、没进回收站）、这个人能看
	if _, err := s.GetMeeting(ctx, user, id); err != nil {
		return nil, err
	}
	raw, err := s.q.GetTranscript(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return []transcript.Segment{}, nil
	}
	if err != nil {
		return nil, err
	}
	segs := []transcript.Segment{}
	return segs, json.Unmarshal(raw, &segs)
}

// Minutes 是纪要的某一个版本，加上这场会议的全部版本记录。
type Minutes struct {
	// 0 表示还没有纪要
	Version  int
	Content  string
	Versions []sqlc.ListMinutesVersionsRow
}

// GetMinutes 返回指定版本的纪要；version 为 0 表示当前版本。
func (s *Service) GetMinutes(ctx context.Context, user sqlc.User, id uuid.UUID, version int) (Minutes, error) {
	m, err := s.GetMeeting(ctx, user, id)
	if err != nil {
		return Minutes{}, err
	}
	out := Minutes{Versions: []sqlc.ListMinutesVersionsRow{}}
	if m.CurrentVersion == 0 {
		return out, nil
	}
	if version == 0 {
		version = int(m.CurrentVersion)
	}
	mv, err := s.q.GetMinutesVersion(ctx, sqlc.GetMinutesVersionParams{MeetingID: id, Version: int32(version)})
	if errors.Is(err, pgx.ErrNoRows) {
		return Minutes{}, notFound("没有这个版本的纪要")
	}
	if err != nil {
		return Minutes{}, err
	}
	out.Version, out.Content = int(mv.Version), mv.Content
	out.Versions, err = s.q.ListMinutesVersions(ctx, id)
	return out, err
}

// EditMinutes 保存手动修改的纪要。不覆盖原文，而是生成一个新版本。
func (s *Service) EditMinutes(ctx context.Context, user sqlc.User, id uuid.UUID, content string) (Minutes, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return Minutes{}, invalid("纪要不能为空")
	}
	if len(content) > 1<<20 {
		return Minutes{}, invalid("纪要太长了")
	}
	err := s.tx(ctx, func(_ pgx.Tx, q *sqlc.Queries) error {
		m, err := q.LockMeeting(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.DeletedAt != nil) {
			return errNoMeeting
		}
		if err != nil {
			return err
		}
		access, err := accessIn(ctx, q, user, m)
		if err != nil {
			return err
		}
		if !access.View {
			return errNoMeeting
		}
		if !access.Edit {
			return errNotAllowed
		}
		if m.Status != "done" {
			return conflict("会议正在处理中，等处理完再编辑纪要")
		}
		if _, err := q.AddMinutesVersion(ctx, sqlc.AddMinutesVersionParams{
			MeetingID: id, Content: content, Source: "edit", Template: "", AuthorID: &user.ID,
		}); err != nil {
			return err
		}
		return search.Reindex(ctx, q, id)
	})
	if err != nil {
		return Minutes{}, err
	}
	return s.GetMinutes(ctx, user, id, 0)
}

// AudioPath 返回录音文件在服务器上的路径，给回放用。
func (s *Service) AudioPath(ctx context.Context, user sqlc.User, id uuid.UUID) (string, error) {
	m, err := s.GetMeeting(ctx, user, id)
	if err != nil {
		return "", err
	}
	if m.AudioKey == "" || m.AudioDeletedAt != nil {
		return "", notFound("这场会议没有录音，或者录音已过保留期")
	}
	return s.store.Path(m.AudioKey), nil
}
