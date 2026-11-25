export interface EditorUploadTab {
  id: string
  composeContext(): { state: string } | null
}

export interface EditorUploadIO {
  clearStagedUpload(sessionId: string): Promise<void>
  stageUpload(sessionId: string, paths: string[]): Promise<void>
  transferOne(tab: EditorUploadTab, path: string): Promise<void>
}

/** 水木 getdata 是行输入：只发 U 会停在提示符，必须再用回车提交命令。 */
export const EDITOR_UPLOAD_TRIGGER = 'U\r'

/** 整批附件传输完毕后，再用一次回车接受发文参数并进入正文编辑器。 */
export const EDITOR_ENTER_TRIGGER = '\r'

/**
 * 水木发文设置页的一次 U 只启动一个单文件 rz 接收会话。逐文件暂存、传输，
 * 不能把整批路径塞给一个会话连续发送；整批结束后的回车由调用方统一发送。
 */
export async function uploadEditorAttachmentsSequentially(
  tab: EditorUploadTab,
  paths: string[],
  io: EditorUploadIO,
  onUploaded: (path: string) => void,
  onProgress?: (done: number, total: number, path: string) => void,
): Promise<void> {
  if (!tab.id) throw new Error('终端会话已断开')
  const unique: string[] = []
  const seen = new Set<string>()
  for (const path of paths) {
    const key = path.toLowerCase()
    if (seen.has(key)) continue
    seen.add(key)
    unique.push(path)
  }
  if (!unique.length) return

  if (tab.composeContext()?.state !== 'post-options') {
    throw new Error('当前已离开 BBS 发文设置页，未继续上传附件')
  }
  await io.clearStagedUpload(tab.id)
  try {
    for (let i = 0; i < unique.length; i++) {
      const path = unique[i]!
      await io.stageUpload(tab.id, [path])
      await io.transferOne(tab, path)
      // batch-done 表示 ZFIN/OO 已按 TCP 顺序发给服务端。下一份 U 或整批后的回车
      // 会排在 OO 后面，由 rz 退出后的 getdata 读取，不应再被屏幕文本识别阻断。
      onUploaded(path)
      onProgress?.(i + 1, unique.length, path)
    }
  } catch (e) {
    await io.clearStagedUpload(tab.id).catch(() => { /* 会话可能已断开 */ })
    throw e
  }
}
