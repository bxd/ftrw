package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeEditorDraft(t *testing.T) {
	d := normalizeEditorDraft(EditorDraft{
		ID:    " draft ",
		Mode:  "other",
		Body:  "第一行\r\n\t第二行\r第三行\x00\x1b\x03\x7f\u0080",
		Title: "  标题  ",
		Attachments: []EditorAttachment{
			{Name: "a.png", Path: "a"},
			{Name: "again", Path: "a"},
			{Name: "", Path: "b"},
		},
	})
	if d.ID != "draft" || d.Mode != "new" || d.Title != "标题" {
		t.Fatalf("归一化字段错误：%+v", d)
	}
	if d.Body != "第一行\n    第二行\n第三行" {
		t.Fatalf("正文换行/控制字符归一化错误：%q", d.Body)
	}
	if got := normalizeEditorDraft(EditorDraft{TransferState: "sending"}).TransferState; got != "sending" {
		t.Fatalf("回传中状态应保留，got %q", got)
	}
	if got := normalizeEditorDraft(EditorDraft{TransferState: "sent", Synced: true}).TransferState; got != "sent" {
		t.Fatalf("已同步草稿应标记为已回传，got %q", got)
	}
	if len(d.Attachments) != 1 || d.Attachments[0].Path != "a" {
		t.Fatalf("附件去重/过滤错误：%+v", d.Attachments)
	}
	if d.CreatedAt <= 0 || d.UpdatedAt <= 0 {
		t.Fatal("时间戳应自动补齐")
	}
}

func TestConfigServiceEditorDraftsAndImage(t *testing.T) {
	store := &Store{dir: t.TempDir()}
	cs := NewConfigService(store)
	if got := cs.ListEditorDrafts(); got == nil || len(got) != 0 {
		t.Fatalf("空草稿列表应为 []，got %#v", got)
	}
	if _, err := cs.SaveEditorDraft(EditorDraft{}); err == nil {
		t.Fatal("空 ID 草稿应被拒绝")
	}
	d := EditorDraft{ID: "one", SiteKey: "bbs", Title: "标题", Body: "正文"}
	d, err := cs.SaveEditorDraft(d)
	if err != nil {
		t.Fatal(err)
	}
	if d.Revision != 1 {
		t.Fatalf("首次保存 revision 应为 1，got %d", d.Revision)
	}
	d.Body = "正文二"
	d, err = cs.SaveEditorDraft(d)
	if err != nil {
		t.Fatal(err)
	}
	got := cs.ListEditorDrafts()
	if len(got) != 1 || got[0].Body != "正文二" {
		t.Fatalf("草稿覆盖失败：%+v", got)
	}
	if err := cs.DeleteEditorDraft("one"); err != nil {
		t.Fatal(err)
	}
	if got := cs.ListEditorDrafts(); len(got) != 0 {
		t.Fatalf("删除后仍有草稿：%+v", got)
	}

	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	path, err := cs.SaveEditorImage("文字图片", base64.StdEncoding.EncodeToString(png))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "文字图片.png" {
		t.Fatalf("图片文件名错误：%s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(data, png) {
		t.Fatalf("图片落盘不一致：err=%v", err)
	}
	if _, err := cs.SaveEditorImage("bad.png", base64.StdEncoding.EncodeToString([]byte("not png"))); err == nil {
		t.Fatal("非 PNG 应被拒绝")
	}
}

func TestConfigServiceAllowsEmptyEditorBody(t *testing.T) {
	store := &Store{dir: t.TempDir()}
	cs := NewConfigService(store)
	draft, err := cs.SaveEditorDraft(EditorDraft{ID: "empty", Title: "空正文", Body: "正文"})
	if err != nil {
		t.Fatal(err)
	}
	draft.Body = ""
	draft, err = cs.SaveEditorDraft(draft)
	if err != nil {
		t.Fatalf("清空正文后应允许保存：%v", err)
	}
	got := cs.ListEditorDrafts()
	if len(got) != 1 || got[0].Body != "" || got[0].Revision != draft.Revision {
		t.Fatalf("空正文草稿未正确保存：%+v", got)
	}
}

func TestSiteAutoEditorDefaultAndExplicitOff(t *testing.T) {
	first, second := DefaultSite(), DefaultSite()
	if !first.AutoEditor {
		t.Fatal("新 BBS profile 应默认自动打开本地编辑器")
	}
	if first.ProfileID == "" || second.ProfileID == "" || first.ProfileID == second.ProfileID {
		t.Fatal("新 profile 应获得各自独立的稳定身份")
	}
	var old Site
	if err := json.Unmarshal([]byte(`{"name":"旧站","hostType":0,"protocolType":0}`), &old); err != nil {
		t.Fatal(err)
	}
	if got := normalizeSite(old); !got.AutoEditor {
		t.Fatal("旧 profile 缺少 autoEditor 时应补为开启")
	}
	var off Site
	if err := json.Unmarshal([]byte(`{"hostType":0,"protocolType":0,"autoEditor":false}`), &off); err != nil {
		t.Fatal(err)
	}
	if got := normalizeSite(off); got.AutoEditor {
		t.Fatal("用户显式关闭后不应被默认值覆盖")
	}
	store := &Store{dir: t.TempDir()}
	cs := NewConfigService(store)
	if err := cs.SaveSite(-1, off); err != nil {
		t.Fatal(err)
	}
	if got := cs.ListSites(); len(got) != 1 || got[0].AutoEditor {
		t.Fatalf("显式关闭保存后应保持关闭：%+v", got)
	}
	if err := store.load(); err != nil {
		t.Fatal(err)
	}
	if got := cs.ListSites(); len(got) != 1 || got[0].AutoEditor {
		t.Fatalf("重新加载后显式关闭仍应保持关闭：%+v", got)
	}
}

func TestSaveSitePreservesProfileIdentity(t *testing.T) {
	store := &Store{dir: t.TempDir()}
	cs := NewConfigService(store)
	initial := DefaultSite()
	initial.Name = "旧名字"
	if err := cs.SaveSite(-1, initial); err != nil {
		t.Fatal(err)
	}
	id := cs.ListSites()[0].ProfileID
	edited := initial
	edited.ProfileID = "前端试图替换的身份"
	edited.Name = "新名字"
	edited.Host = "new.example.com"
	edited.UserName = "另一个账号"
	if err := cs.SaveSite(0, edited); err != nil {
		t.Fatal(err)
	}
	if err := store.load(); err != nil {
		t.Fatal(err)
	}
	got := cs.ListSites()
	if len(got) != 1 || got[0].ProfileID != id || got[0].Name != "新名字" || got[0].UserName != "另一个账号" {
		t.Fatalf("改名/换主机/换账号后 profileId 应保持不变：%+v", got)
	}
}

func TestStoreMigratesOldSitesWithoutDisablingAutoEditor(t *testing.T) {
	dir := t.TempDir()
	raw := `[{"name":"甲","host":"bbs.example","port":23,"hostType":0,"protocolType":0,"userName":"a"},{"name":"乙","host":"bbs.example","port":23,"hostType":0,"protocolType":0,"userName":"b","autoEditor":false}]`
	if err := os.WriteFile(filepath.Join(dir, "sites.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &Store{dir: dir}
	if err := store.load(); err != nil {
		t.Fatal(err)
	}
	if len(store.Sites) != 2 || !store.Sites[0].AutoEditor || store.Sites[1].AutoEditor {
		t.Fatalf("旧站默认开启、显式关闭保持关闭失败：%+v", store.Sites)
	}
	if store.Sites[0].ProfileID == "" || store.Sites[1].ProfileID == "" || store.Sites[0].ProfileID == store.Sites[1].ProfileID {
		t.Fatalf("同端点不同账号应获得不同稳定身份：%+v", store.Sites)
	}
	if err := store.load(); err != nil {
		t.Fatal(err)
	}
	if !store.Sites[0].AutoEditor || store.Sites[1].AutoEditor || store.Sites[0].ProfileID == store.Sites[1].ProfileID {
		t.Fatalf("迁移结果不稳定：%+v", store.Sites)
	}
}

func TestSaveEditorDraftRejectsStaleRevision(t *testing.T) {
	store := &Store{dir: t.TempDir()}
	cs := NewConfigService(store)
	base := EditorDraft{ID: "one", Body: "旧内容"}
	first, err := cs.SaveEditorDraft(base)
	if err != nil {
		t.Fatal(err)
	}
	newer := first
	newer.Body = "新内容"
	newer, err = cs.SaveEditorDraft(newer)
	if err != nil {
		t.Fatal(err)
	}
	stale := first
	stale.Body = "迟到的旧内容"
	if _, err := cs.SaveEditorDraft(stale); err == nil {
		t.Fatal("旧 revision 不应覆盖新快照")
	}
	got := cs.ListEditorDrafts()
	if len(got) != 1 || got[0].Body != "新内容" || got[0].Revision != newer.Revision {
		t.Fatalf("旧快照覆盖了新草稿：%+v", got)
	}
	if err := cs.DeleteEditorDraft("one"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.SaveEditorDraft(newer); err == nil {
		t.Fatal("删除后旧快照不应重新创建草稿")
	}
}

func TestStoreBacksUpCorruptDrafts(t *testing.T) {
	dir := t.TempDir()
	raw := `{"drafts":`
	if err := os.WriteFile(filepath.Join(dir, "drafts.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &Store{dir: dir}
	if err := store.load(); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(filepath.Join(dir, "drafts.corrupt-*.json"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("损坏草稿应保留备份：backups=%v err=%v", backups, err)
	}
	got, err := os.ReadFile(backups[0])
	if err != nil || string(got) != raw {
		t.Fatalf("备份内容不一致：%q err=%v", got, err)
	}
}

func TestSenderRejectsRemoteAbort(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "ok.txt")
	if err := os.WriteFile(file, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	conn := &fakeZConn{in: cat(tHexHdr(zabort, 0))}
	e := &zmodemEngine{
		sess: &Session{ID: "test"}, rwc: conn, direction: "send", upload: []string{file},
	}
	if err := e.sender(); !errors.Is(err, errRemoteAbort) {
		t.Fatalf("远端中止应终止整批，got %v", err)
	}
	if strings.Contains(string(conn.out), "OO") {
		t.Fatal("远端中止后不应发送批次成功收尾")
	}
}

func TestSenderRejectsUnconfirmedEOF(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "ok.txt")
	data := []byte("data")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// 对端允许从 EOF 续传，但随后断开且没有 ZRINIT 完成确认。
	conn := &fakeZConn{in: cat(tHexHdr(zrpos, uint32(len(data))))}
	e := &zmodemEngine{
		sess: &Session{ID: "test"}, rwc: conn, direction: "send", upload: []string{file},
	}
	if err := e.sender(); err == nil || errors.Is(err, errRemoteAbort) {
		t.Fatalf("未确认 EOF 应报告上传失败，got %v", err)
	}
	if strings.Contains(string(conn.out), "OO") {
		t.Fatal("文件完成未确认时不应发送批次成功收尾")
	}
}

func TestUploadBatchMessage(t *testing.T) {
	cases := []struct {
		ok, skip, fail int
		want           string
	}{
		{2, 0, 0, "上传完成"},
		{1, 1, 1, "上传完成（2 个文件被跳过/失败）"},
		{0, 0, 2, "上传失败（2 个文件被跳过/失败）"},
	}
	for _, tc := range cases {
		if got := uploadBatchMessage(tc.ok, tc.skip, tc.fail); got != tc.want {
			t.Errorf("uploadBatchMessage(%d,%d,%d)=%q, want %q", tc.ok, tc.skip, tc.fail, got, tc.want)
		}
	}
}

func TestSenderContinuesAfterFileFailure(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "ok.txt")
	if err := os.WriteFile(good, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 第二个文件由服务器跳过；随后对端确认 ZFIN。首个文件缺失不应中止整批。
	conn := &fakeZConn{in: cat(tHexHdr(zskip, 0), tHexHdr(zfin, 0))}
	e := &zmodemEngine{
		sess: &Session{ID: "test"}, rwc: conn, direction: "send",
		upload: []string{filepath.Join(dir, "missing.txt"), good},
	}
	if err := e.sender(); err != nil {
		t.Fatal(err)
	}
	if out := string(conn.out); !strings.Contains(out, "ok.txt") || !strings.HasSuffix(out, "OO") {
		t.Fatalf("首文件失败后未继续发送后续文件或未完成断会：%q", out)
	}
}

func TestResidualZmodemFrameKeepsStagedUpload(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "next.png")
	if err := os.WriteFile(file, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Session{
		ID: "test", Site: Site{EnableZmodem: true}, conn: &fakeZConn{},
		stagedUpload: []string{file},
	}
	ts := &TerminalService{store: &Store{dir: dir}}
	ts.runZmodem(s, tHexHdr(zfin, 0))

	got := s.takeStagedUpload()
	if len(got) != 1 || got[0] != file {
		t.Fatalf("残留帧不应消费下一份预选附件: %v", got)
	}
	if s.zmCooldown.Load() != 0 {
		t.Fatal("残留帧不应触发冷却并丢弃后续真正的 ZRINIT")
	}
	if s.zmDrain.Load() {
		t.Fatal("残留帧不应进入二进制丢弃状态")
	}
}

func TestZmodemRunReturnsTrailingBbsRedraw(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "ok.png")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tail := []byte("\x1b[H\x1b[2J发表文章于 Test 讨论区\r\n使用标题: 实测\r\n")
	stream := cat(
		tHexHdr(zrinit, 0), // BBS 启动 rz
		tHexHdr(zrinit, 0), // 回应 ZRQINIT
		tHexHdr(zrpos, 0),  // 接受 ZFILE
		tHexHdr(zrinit, 0), // 空文件 ZEOF 完成
		tBin32Hdr(zfin, 0), // 对端确认断会
		tail,               // 与 ZFIN 同包到达的 BBS 重绘
	)
	s := &Session{ID: "test", conn: &fakeZConn{}, stagedUpload: []string{file}}
	ts := &TerminalService{store: &Store{dir: dir}}
	if got := ts.runZmodem(s, stream); string(got) != string(tail) {
		t.Fatalf("ZMODEM 收尾后的 BBS 重绘必须回放，got %q want %q", got, tail)
	}
}

func TestZmodemRunConsumesStagedUploadOnZRInit(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "ok.txt")
	if err := os.WriteFile(file, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	conn := &fakeZConn{in: cat(tHexHdr(zrinit, 0), tHexHdr(zrinit, 0), tHexHdr(zabort, 0))}
	s := &Session{ID: "test", stagedUpload: []string{file}}
	e := &zmodemEngine{sess: s, rwc: conn, direction: "recv"}
	if err := e.run(); !errors.Is(err, errRemoteAbort) {
		t.Fatalf("测试应在 ZFILE 阶段被远端中止, got %v", err)
	}
	if got := s.takeStagedUpload(); len(got) != 0 {
		t.Fatalf("合法 ZRINIT 应消费预选附件: %v", got)
	}
	if out := string(conn.out); !strings.Contains(out, "ok.txt") {
		t.Fatalf("未使用预选附件发送 ZFILE: %q", out)
	}
}

func TestStageUploadValidatesBeforeSaving(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "附件.png")
	if err := os.WriteFile(file, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := &TerminalService{sessions: map[string]*Session{
		"s": {ID: "s", Site: Site{EnableZmodem: true}},
	}}
	if err := ts.StageUpload("s", []string{file, filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("批量中包含缺失文件时应整批拒绝")
	}
	if got := ts.sessions["s"].takeStagedUpload(); len(got) != 0 {
		t.Fatalf("校验失败不应留下预选附件：%v", got)
	}
	if err := ts.StageUpload("s", []string{file, file}); err != nil {
		t.Fatal(err)
	}
	got := ts.sessions["s"].takeStagedUpload()
	if len(got) != 1 || !strings.EqualFold(got[0], file) {
		t.Fatalf("预选附件应去重：%v", got)
	}
	if got := ts.sessions["s"].takeStagedUpload(); len(got) != 0 {
		t.Fatalf("取走后应清空：%v", got)
	}
}
