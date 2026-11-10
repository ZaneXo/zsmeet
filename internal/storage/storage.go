// Package storage 管理录音和上传分片。一期只有本地磁盘；
// 换 S3/MinIO 时在这里加实现，worker 需要本地文件时先下载到临时目录。
package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

type Local struct{ root string }

func NewLocal(root string) (*Local, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("建不了数据目录 %s：%w", root, err)
	}
	return &Local{root: root}, nil
}

func (s *Local) chunkDir(meeting uuid.UUID) string {
	return filepath.Join(s.root, "chunks", meeting.String())
}

// ChunkPath 第 seq 片的位置。序号补零，按文件名排序就是上传顺序。
func (s *Local) ChunkPath(meeting uuid.UUID, seq int) string {
	return filepath.Join(s.chunkDir(meeting), fmt.Sprintf("%06d", seq))
}

// PutChunk 写入一片（先写临时文件再改名，重复上传同一片是安全的），返回字节数。
func (s *Local) PutChunk(meeting uuid.UUID, seq int, r io.Reader) (int64, error) {
	dir := s.chunkDir(meeting)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(dir, ".part-*")
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return 0, err
	}
	return n, os.Rename(tmp.Name(), s.ChunkPath(meeting, seq))
}

// RemoveChunks 拼好整段录音后删掉分片。
func (s *Local) RemoveChunks(meeting uuid.UUID) error {
	return os.RemoveAll(s.chunkDir(meeting))
}

// AudioKey 拼好、转码后的录音在存储里的名字。
func AudioKey(meeting uuid.UUID) string {
	return filepath.Join("audio", meeting.String()+".m4a")
}

// Path 把存储里的名字变成本地路径。
func (s *Local) Path(key string) string {
	return filepath.Join(s.root, key)
}

// TempDir 给 worker 处理一场会议用的临时目录，用完调用方删掉。
func (s *Local) TempDir(meeting uuid.UUID) (string, error) {
	dir := filepath.Join(s.root, "tmp", meeting.String())
	return dir, os.MkdirAll(dir, 0o755)
}
