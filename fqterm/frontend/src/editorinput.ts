export const MAX_EDITOR_ATTACHMENTS = 20

type EditorShortcut = Pick<KeyboardEvent, 'key' | 'ctrlKey' | 'metaKey' | 'altKey' | 'shiftKey'>

function matchesShortcut(e: EditorShortcut, key: string): boolean {
  return (e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === key
}

// 保存快捷键只落草稿，不触发回传或关闭编辑器。
export function isSaveShortcut(e: EditorShortcut): boolean {
  return matchesShortcut(e, 's')
}

// 回传快捷键与底栏回传按钮共用同一个提交入口。
export function isHandoffShortcut(e: EditorShortcut): boolean {
  return matchesShortcut(e, 'w')
}

export function attachmentPathKey(path: string): string {
  return path.trim().toLowerCase()
}

// 文件选择、剪贴板文件、拖放文件共用同一套去重规则；数量上限由编辑器统一提示。
export function newAttachmentPaths(paths: string[], existing: { path: string }[]): string[] {
  const seen = new Set(existing.map((item) => attachmentPathKey(item.path)))
  const out: string[] = []
  for (const raw of paths || []) {
    const path = raw.trim()
    const key = attachmentPathKey(path)
    if (!path || seen.has(key)) continue
    seen.add(key)
    out.push(path)
  }
  return out
}

// 剪贴板图片只生成临时 PNG 附件，文件名保持纯 ASCII，避免编码和上传兼容问题。
export function clipboardImageFileName(now: Date): string {
  const stamp = `${now.getFullYear()}${String(now.getMonth() + 1).padStart(2, '0')}${String(now.getDate()).padStart(2, '0')}-${String(now.getHours()).padStart(2, '0')}${String(now.getMinutes()).padStart(2, '0')}${String(now.getSeconds()).padStart(2, '0')}-${String(now.getMilliseconds()).padStart(3, '0')}`
  return `clipboard-image-${stamp}.png`
}
