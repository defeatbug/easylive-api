package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMigrationFilesSkipsJunk 钉住一个真实事故：
// macOS 复制文件到非 APFS 卷会生成 "._001_init.sql" 这类伴生文件，
// 它同样以 .sql 结尾，被当成迁移执行后报 "invalid message format (SQLSTATE 08P01)"，
// 而且排序时排在真正的迁移**前面**——于是真正的迁移一个都没跑成。
func TestMigrationFilesSkipsJunk(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"001_init.sql", "._001_init.sql", "_hidden.sql", "002_rooms.sql", ".DS_Store", "readme.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("select 1;"), 0o644); err != nil {
			t.Fatalf("准备文件失败: %v", err)
		}
	}
	got, err := migrationFiles(dir)
	if err != nil {
		t.Fatalf("列出迁移失败: %v", err)
	}
	want := []string{filepath.Join(dir, "001_init.sql"), filepath.Join(dir, "002_rooms.sql")}
	if len(got) != len(want) {
		t.Fatalf("应只保留真正的迁移文件 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个应为 %s，实际 %s（排序也要正确）", i, want[i], got[i])
		}
	}

	// 目录里只有垃圾文件时，应明确报错而不是"执行 0 个文件"蒙混过关
	junk := t.TempDir()
	if err := os.WriteFile(filepath.Join(junk, "._x.sql"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := migrationFiles(junk); err == nil {
		t.Fatalf("只有垃圾文件时应报错")
	}
}
