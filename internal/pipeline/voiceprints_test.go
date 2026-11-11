package pipeline

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/service"
	"github.com/ZaneXo/zsmeet/internal/settings"
	"github.com/ZaneXo/zsmeet/internal/voiceprint"
)

// 假引擎里两位说话人的声音：说话人 1（编号 0）朝 x 轴，说话人 2（编号 1）朝 y 轴
var voiceX, voiceY = []float32{1, 0, 0}, []float32{0, 1, 0}

// voiceprint 往声纹库里加一个人，样本来自另一场会议。返回声纹的 ID。
func (e *env) voiceprint(name string, sample []float32) uuid.UUID {
	e.t.Helper()
	ctx := e.t.Context()
	other, err := e.q.CreateMeeting(ctx, sqlc.CreateMeetingParams{
		OwnerID: e.user.ID, Title: "以前的会", Source: "upload", Template: "general", Visibility: "team",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	var id uuid.UUID
	if err := e.pool.QueryRow(ctx, "INSERT INTO voiceprints (name) VALUES ($1) RETURNING id", name).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, "INSERT INTO voiceprint_samples (meeting_id, speaker, voiceprint_id, embedding, seconds) VALUES ($1, 0, $2, $3, 20)",
		other.ID, id, voiceprint.Normalize(sample)); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) enableVoiceprints(on bool) {
	e.t.Helper()
	if err := e.w.settings.SaveVoiceprints(e.t.Context(), settings.Voiceprints{Enabled: on}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.t.Context(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// 声纹关着（默认）：不算、不存任何声音特征，也不按声音认人。
func TestVoiceprintsDisabledStoresNothing(t *testing.T) {
	e := newEnv(t)
	e.engine.voices = map[int][]float32{0: voiceX, 1: voiceY}
	e.voiceprint("王悦", []float32{0.1, 1, 0}) // 以前打开时记下的
	args := e.upload(wav())
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	m := e.meeting(args.MeetingID)
	if m.Status != "done" {
		t.Fatalf("会议应该处理完成：%s %s", m.Status, m.Error)
	}
	if n := e.engine.embedded.Load(); n != 0 {
		t.Errorf("关着时不该算声音特征，算了 %d 次", n)
	}
	if n := e.count("SELECT count(*) FROM speaker_embeddings"); n != 0 {
		t.Errorf("关着时不该存声音特征，存了 %d 条", n)
	}
	if string(m.Speakers) != "{}" || len(m.AutoSpeakers) != 0 {
		t.Errorf("关着时不按声音认人：%s %v", m.Speakers, m.AutoSpeakers)
	}
}

// 声纹打开：转写完按声音认出声纹库里的人，名字在写纪要之前定下来；自动认出的不当成样本记进声纹库。
func TestVoiceprintsNameSpeakersBeforeMinutes(t *testing.T) {
	e := newEnv(t)
	e.llm(nil)
	e.enableVoiceprints(true)
	e.engine.voices = map[int][]float32{0: voiceX, 1: voiceY}
	e.voiceprint("王悦", []float32{0.1, 1, 0})
	e.voiceprint("张敏", []float32{0, 0, 1}) // 和谁都不像
	args := e.upload(wav())
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}

	// 算声纹用的是转写里的编号（说话人区分给的 4 和 9 按开口顺序换成 0 和 1）
	if got := e.engine.turns; len(got) != 2 || got[0].Speaker != 0 || got[1].Speaker != 1 {
		t.Errorf("算声纹的时间段要换成转写里的编号：%+v", got)
	}
	m := e.meeting(args.MeetingID)
	if m.Status != "done" || string(m.Speakers) != `{"1": "王悦"}` || !slices.Equal(m.AutoSpeakers, []int32{1}) {
		t.Fatalf("说话人 2 应该按声音认成王悦，并记下是自动的：%s %s %v", m.Status, m.Speakers, m.AutoSpeakers)
	}
	if prompt := e.lastPrompt.Load(); prompt == nil || !strings.Contains(*prompt, "王悦：同意，录音器放到二期。") {
		t.Errorf("大模型要看到认出来的名字：%v", prompt)
	}
	if n := e.count("SELECT count(*) FROM speaker_embeddings WHERE meeting_id = $1", m.ID); n != 2 {
		t.Errorf("两位说话人的声音特征都要存下来，存了 %d 条", n)
	}
	if n := e.count("SELECT count(*) FROM voiceprint_samples WHERE meeting_id = $1", m.ID); n != 0 {
		t.Errorf("自动认出的不能记成样本，记了 %d 条", n)
	}
}

// 算声音特征的时候管理员把声纹关了：一点都不存，也不认人。
func TestVoiceprintsTurnedOffWhileProcessing(t *testing.T) {
	e := newEnv(t)
	e.enableVoiceprints(true)
	e.engine.voices = map[int][]float32{0: voiceX, 1: voiceY}
	e.engine.onEmbed = func() { e.enableVoiceprints(false) }
	e.voiceprint("王悦", []float32{0.1, 1, 0})
	args := e.upload(wav())
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	m := e.meeting(args.MeetingID)
	if e.engine.embedded.Load() != 1 || m.Status != "done" {
		t.Fatalf("应该算过一次、会议照常完成：%d %s", e.engine.embedded.Load(), m.Status)
	}
	if n := e.count("SELECT count(*) FROM speaker_embeddings"); n != 0 || string(m.Speakers) != "{}" {
		t.Errorf("关掉以后不存、不认人：%d 条，名字 %s", n, m.Speakers)
	}
}

// 重新转写：以前起的名字、按声音认出的标记、记到声纹库里的样本都跟着人挪到新编号上；
// 已经有的名字不被按声音认出的覆盖，同一个声纹在一场会议里只用一次。
// 对不上的样本删掉，它的声纹空了也删掉；这次没动过的空声纹不删。
func TestVoiceprintsFollowRetranscribe(t *testing.T) {
	e := newEnv(t)
	e.enableVoiceprints(true)
	e.engine.voices = map[int][]float32{0: voiceX, 1: voiceY}
	e.voiceprint("王悦", []float32{0.1, 1, 0})
	args := e.upload(wav())
	if err := e.work(args); err != nil {
		t.Fatal(err)
	}
	id := args.MeetingID
	ctx := t.Context()

	// 以前的转写里两个人的编号和现在的相反：0 是现在的 1，1 是现在的 0。
	// 编号 1 是按声音认出的王悦，编号 0 是人起的李航，样本记在李航名下
	// 以前还有一位编号 2 的赵磊，样本只有这一个，新的转写里对不上他；张敏是声纹库里一个没有样本、这次也不会动到的声纹
	var lihang, zhaolei uuid.UUID
	if err := e.pool.QueryRow(ctx, "INSERT INTO voiceprints (name) VALUES ('李航') RETURNING id").Scan(&lihang); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, "INSERT INTO voiceprints (name) VALUES ('赵磊') RETURNING id").Scan(&zhaolei); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, "INSERT INTO voiceprints (name) VALUES ('张敏')"); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE transcripts SET segments = '[{"start":0,"end":1,"speaker":1,"text":"一期只做网页端。"},{"start":1,"end":2,"speaker":0,"text":"同意。"},{"start":100,"end":101,"speaker":2,"text":"好。"}]' WHERE meeting_id = $1`,
		`UPDATE meetings SET speakers = '{"0": "李航", "1": "王悦"}', auto_speakers = '{1}' WHERE id = $1`,
		`INSERT INTO voiceprint_samples (meeting_id, speaker, voiceprint_id, embedding, seconds) VALUES ($1, 0, '` + lihang.String() + `', '{0,0,1}', 3)`,
		`INSERT INTO voiceprint_samples (meeting_id, speaker, voiceprint_id, embedding, seconds) VALUES ($1, 2, '` + zhaolei.String() + `', '{0,1,1}', 3)`,
	} {
		if _, err := e.pool.Exec(ctx, stmt, id); err != nil {
			t.Fatal(err)
		}
	}
	a := e.enqueue(id, "done")
	if err := e.q.MarkRetranscribe(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.work(a); err != nil {
		t.Fatal(err)
	}

	m := e.meeting(id)
	// 现在的 1 是李航（王悦的声纹最像他，但他已经有名字了，不覆盖）；现在的 0 还是自动认出的王悦
	if string(m.Speakers) != `{"0": "王悦", "1": "李航"}` || !slices.Equal(m.AutoSpeakers, []int32{0}) {
		t.Errorf("名字和自动的标记要跟着人走：%s %v", m.Speakers, m.AutoSpeakers)
	}
	var speaker int
	var embedding []float32
	if err := e.pool.QueryRow(ctx, "SELECT speaker, embedding FROM voiceprint_samples WHERE meeting_id = $1", id).Scan(&speaker, &embedding); err != nil {
		t.Fatal(err)
	}
	if speaker != 1 || !slices.Equal(embedding, voiceY) {
		t.Errorf("李航的样本要挪到新编号 1，换成新算的声音特征：%d %v", speaker, embedding)
	}
	rows, err := e.pool.Query(ctx, "SELECT name FROM voiceprints ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names, ","); got != "张敏,李航,王悦" {
		t.Errorf("赵磊的样本对不上删掉、声纹空了删掉；没动过的张敏留着：%s", got)
	}
}

// 转写完存说话人的声音特征、挪样本之前要先拿声纹库的锁：别的事务（改名、删声纹）拿着锁时等着，放开以后照常做完。
func TestVoiceprintsWaitForLibraryLock(t *testing.T) {
	e := newEnv(t)
	e.enableVoiceprints(true)
	e.engine.voices = map[int][]float32{0: voiceX, 1: voiceY}
	args := e.upload(wav())
	ctx := t.Context()

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// 声纹库的锁（service.voiceprintsLock）
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(7368824)"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.work(args) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := e.pool.QueryRow(ctx,
			"SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objid = 7368824 AND NOT granted").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("锁被别人拿着时不该处理完：%v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("等了 10 秒也没在声纹库的锁上排队")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("锁放开以后应该处理完")
	}
	if n := e.count("SELECT count(*) FROM speaker_embeddings WHERE meeting_id = $1", args.MeetingID); n != 2 {
		t.Errorf("放开以后照常存声音特征：%d", n)
	}
}

// 转写完的事务已经在声纹库的锁里读到「打开」、正要存声音特征时，管理员关掉声纹：关掉要等这个事务做完才返回，
// 关掉成功以后不会再有声音特征存进来。
func TestVoiceprintsTurnOffWaitsForRunningSave(t *testing.T) {
	e := newEnv(t)
	e.enableVoiceprints(true)
	e.engine.voices = map[int][]float32{0: voiceX, 1: voiceY}
	args := e.upload(wav())
	ctx := t.Context()
	svc := service.New(e.pool, nil, e.w.settings, nil)

	// 先放一条旧的声音特征并锁住这一行：转写完的事务拿到声纹库的锁、读完设置以后，删旧特征时卡在这里
	if _, err := e.pool.Exec(ctx, "INSERT INTO speaker_embeddings (meeting_id, speaker, embedding, seconds) VALUES ($1, 0, '{1,0,0}', 3)", args.MeetingID); err != nil {
		t.Fatal(err)
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "SELECT 1 FROM speaker_embeddings WHERE meeting_id = $1 FOR UPDATE", args.MeetingID); err != nil {
		t.Fatal(err)
	}
	worked := make(chan error, 1)
	go func() { worked <- e.work(args) }()
	e.waitUntil("SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%DELETE FROM speaker_embeddings WHERE meeting_id%'", worked)

	// 这时关掉：转写完的事务拿着声纹库的锁，关掉要排队
	turnedOff := make(chan error, 1)
	go func() {
		_, err := svc.SaveVoiceprintSettings(context.Background(), false)
		turnedOff <- err
	}()
	e.waitUntil("SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objid = 7368824 AND NOT granted", turnedOff)

	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []chan error{worked, turnedOff} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("放开以后应该都做完")
		}
	}
	// 转写在关掉之前读到的是打开，存下的声音特征算关掉之前的（关掉不删已有的）；关掉以后设置是关着
	if n := e.count("SELECT count(*) FROM speaker_embeddings WHERE meeting_id = $1", args.MeetingID); n != 2 {
		t.Errorf("转写的事务在关掉之前做完，存了 %d 条", n)
	}
	if cfg, err := e.w.settings.Voiceprints(ctx); err != nil || cfg.Enabled {
		t.Errorf("应该关掉了：%v %v", cfg, err)
	}
}

// waitUntil 等到 sql 数出来的行数大于 0（比如有人在某把锁上排队）；done 先有结果说明本该排队的操作没排队就做完了。
func (e *env) waitUntil(sql string, done <-chan error) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if e.count(sql) > 0 {
			return
		}
		select {
		case err := <-done:
			e.t.Fatalf("该排队等着的操作没等就做完了：%v", err)
		default:
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("等了 10 秒也没等到：%s", sql)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
