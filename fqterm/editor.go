package main

import (
	"strings"
	"time"
)

// EditorAttachment 是本地编辑器里待上传的一个附件。Path 即 ZMODEM 发送时使用的
// 本地路径；Generated 区分文字转图片生成的副本与用户直接选择的原文件。
type EditorAttachment struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Generated bool   `json:"generated"`
}

// EditorDraft 是一篇尚未提交的本地文章。站点、版面、标题与模式用来在草稿列表里
// 区分文章；Body 始终使用 \n，送回 BBS 时再由终端层逐行转换成回车。
type EditorDraft struct {
	ID          string             `json:"id"`
	ProfileID   string             `json:"profileId,omitempty"`
	Revision    int64              `json:"revision"`
	SiteKey     string             `json:"siteKey"`
	SiteName    string             `json:"siteName"`
	Mode        string             `json:"mode"`
	Board       string             `json:"board"`
	Title       string             `json:"title"`
	Body        string             `json:"body"`
	Quote       string             `json:"quote,omitempty"`
	Attachments []EditorAttachment `json:"attachments"`
	CreatedAt   int64              `json:"createdAt"`
	UpdatedAt   int64              `json:"updatedAt"`
	Synced      bool               `json:"synced"`
	// TransferState 在首次写入远端前落盘；关闭重开后仍能阻止重复灌入整篇或半篇正文。
	TransferState string `json:"transferState,omitempty"`
}

func normalizeEditorDraft(d EditorDraft) EditorDraft {
	d.ID = strings.TrimSpace(d.ID)
	d.ProfileID = strings.TrimSpace(d.ProfileID)
	if d.Revision < 0 {
		d.Revision = 0
	}
	d.SiteKey = strings.TrimSpace(d.SiteKey)
	d.SiteName = strings.TrimSpace(d.SiteName)
	d.Board = strings.TrimSpace(d.Board)
	d.Title = strings.TrimSpace(d.Title)
	if d.Mode != "reply" {
		d.Mode = "new"
	}
	d.Body = normalizeEditorBody(d.Body)
	d.TransferState = strings.TrimSpace(d.TransferState)
	if d.Synced {
		d.TransferState = "sent"
	} else if d.TransferState != "sending" && d.TransferState != "sent" {
		d.TransferState = ""
	}
	now := time.Now().UnixMilli()
	if d.CreatedAt <= 0 {
		d.CreatedAt = now
	}
	if d.UpdatedAt <= 0 {
		d.UpdatedAt = d.CreatedAt
	}
	atts := make([]EditorAttachment, 0, len(d.Attachments))
	seen := make(map[string]bool, len(d.Attachments))
	for _, a := range d.Attachments {
		a.Name = strings.TrimSpace(a.Name)
		a.Path = strings.TrimSpace(a.Path)
		if a.Name == "" || a.Path == "" || seen[a.Path] {
			continue
		}
		seen[a.Path] = true
		atts = append(atts, a)
	}
	d.Attachments = atts
	return d
}

// normalizeEditorBody 与前端 normalizeLines 保持同一套安全规则：TAB 展开为空格，
// 其余 C0/C1 控制字符全部删除，避免粘贴内容被 BBS 编辑器当成中止、退格等真实按键。
func normalizeEditorBody(body string) string {
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case r == '\n':
			return r
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			return -1
		default:
			return r
		}
	}, strings.ReplaceAll(body, "\t", "    "))
}
