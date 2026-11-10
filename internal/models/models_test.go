package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// useFakeFiles 把要下载的文件清单换成两个小文件，由测试服务器提供。
func useFakeFiles(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body []byte)) (mirror string) {
	t.Helper()
	bodies := map[string][]byte{
		"repo/resolve/main/a.onnx": []byte(strings.Repeat("a", 4096)),
		"repo/resolve/main/b.txt":  []byte("tokens"),
	}
	original := files
	t.Cleanup(func() { files = original })
	files = nil
	for path, body := range bodies {
		sum := sha256.Sum256(body)
		files = append(files, file{name: filepath.Base(path), path: path, size: int64(len(body)), sha256: hex.EncodeToString(sum[:])})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		handler(w, r, body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestEnsureDownloadsOnceAndVerifies(t *testing.T) {
	var requests atomic.Int32
	mirror := useFakeFiles(t, func(w http.ResponseWriter, _ *http.Request, body []byte) {
		requests.Add(1)
		w.Write(body)
	})
	dir := t.TempDir()

	var last, total int64
	if err := Ensure(t.Context(), dir, mirror, func(d, tot int64) { last, total = d, tot }); err != nil {
		t.Fatal(err)
	}
	if last != total || total != 4096+6 {
		t.Errorf("进度应该走到头：%d / %d", last, total)
	}
	for _, f := range files {
		if info, err := os.Stat(filepath.Join(dir, f.name)); err != nil || info.Size() != f.size {
			t.Errorf("%s 没有下载好：%v", f.name, err)
		}
	}

	// 文件都在了，再调用一次不应该再下载
	if err := Ensure(t.Context(), dir, mirror, nil); err != nil {
		t.Fatal(err)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("两个文件应该各下载一次，实际请求了 %d 次", n)
	}
}

func TestEnsureRejectsTamperedFile(t *testing.T) {
	mirror := useFakeFiles(t, func(w http.ResponseWriter, _ *http.Request, body []byte) {
		// 大小一样，内容被换掉了
		w.Write([]byte(strings.Repeat("x", len(body))))
	})
	dir := t.TempDir()

	// 镜像和备用地址都指向这个被篡改的服务器，免得测试去连真的下载站
	err := fetchMissingFrom(t.Context(), dir, []string{mirror}, func(int64, int64) {})
	if err == nil || !strings.Contains(err.Error(), "校验和不对") {
		t.Fatalf("内容被篡改的文件应该被拒绝，得到 %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("校验失败的文件不应该留在模型目录里，还剩 %d 个", len(entries))
	}
}

// 第二个调用者不重复下载，而是等第一个，并且能看到进度。
func TestEnsureFollowersShareOneDownload(t *testing.T) {
	var requests atomic.Int32
	release := make(chan struct{})
	mirror := useFakeFiles(t, func(w http.ResponseWriter, _ *http.Request, body []byte) {
		requests.Add(1)
		w.Write(body[:len(body)/2])
		w.(http.Flusher).Flush()
		<-release
		w.Write(body[len(body)/2:])
	})
	dir := t.TempDir()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	var followerSawProgress atomic.Bool
	wg.Go(func() { errs[0] = Ensure(t.Context(), dir, mirror, nil) })
	// 等第一个开始下载，再让第二个进来
	for requests.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	wg.Go(func() {
		errs[1] = Ensure(t.Context(), dir, mirror, func(done, total int64) {
			if done > 0 && done < total {
				followerSawProgress.Store(true)
			}
		})
	})
	time.Sleep(700 * time.Millisecond)
	close(release)
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("两个调用者应该共用一次下载（2 个文件 2 次请求），实际 %d 次", n)
	}
	if !followerSawProgress.Load() {
		t.Error("后来的调用者应该能看到下载进度")
	}
}

func TestEnsureStopsWhenCancelled(t *testing.T) {
	mirror := useFakeFiles(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Write(body[:1])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := fetchMissingFrom(ctx, t.TempDir(), []string{mirror}, func(int64, int64) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("取消后应该马上停下并返回取消的原因，得到 %v", err)
	}
}

// 第一个下载源连不上时换下一个；之后的文件直接用能连上的那个，不再去碰坏的。
func TestFallsBackAndRemembersWorkingHost(t *testing.T) {
	good := useFakeFiles(t, func(w http.ResponseWriter, _ *http.Request, body []byte) { w.Write(body) })
	var badHits atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badHits.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	t.Cleanup(bad.Close)

	dir := t.TempDir()
	if err := fetchMissingFrom(t.Context(), dir, []string{bad.URL, good}, func(int64, int64) {}); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(dir, f.name)); err != nil {
			t.Errorf("%s 没有下载下来：%v", f.name, err)
		}
	}
	if n := badHits.Load(); n != 1 {
		t.Errorf("坏的下载源应该只被试 1 次（第一个文件），实际 %d 次", n)
	}
}
