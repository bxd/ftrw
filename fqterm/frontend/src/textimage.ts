export interface TextImageOptions {
  text: string
  width: number
  fontSize: number
  foreground: string
  background: string
  padding: number
  lineGap: number
}

export interface TextImageResult {
  dataUrl: string
  width: number
  height: number
}

export const TEXT_IMAGE_DEFAULTS: Omit<TextImageOptions, 'text'> = {
  width: 800,
  fontSize: 24,
  foreground: '#ffffff',
  background: '#000000',
  padding: 28,
  lineGap: 8,
}

/** 附件名只使用 ASCII，避免 BBS 页面和 ZMODEM 链路处理中文文件名。 */
export function textImageFileName(now: Date): string {
  const stamp = `${now.getFullYear()}${String(now.getMonth() + 1).padStart(2, '0')}${String(now.getDate()).padStart(2, '0')}-${String(now.getHours()).padStart(2, '0')}${String(now.getMinutes()).padStart(2, '0')}${String(now.getSeconds()).padStart(2, '0')}`
  return `text-image-${stamp}`
}

/** 按显示宽度折行；中文与 ASCII 都逐码点测量，避免把代理对拆成两半。 */
function wrapLine(ctx: CanvasRenderingContext2D, text: string, maxWidth: number): string[] {
  if (!text) return ['']
  const out: string[] = []
  let line = ''
  for (const ch of Array.from(text)) {
    if (ch === '\t') {
      const next = line + '    '
      if (ctx.measureText(next).width > maxWidth && line) {
        out.push(line)
        line = '    '
      } else line = next
      continue
    }
    const next = line + ch
    if (line && ctx.measureText(next).width > maxWidth) {
      out.push(line)
      line = ch
    } else line = next
  }
  out.push(line)
  return out
}

function clamp(value: number, min: number, max: number, fallback: number): number {
  return Number.isFinite(value) ? Math.max(min, Math.min(max, Math.round(value))) : fallback
}

/** Canvas 生成 PNG data URL；宽度是折行上限，实际画布按最长行收缩。 */
export async function renderTextImage(options: TextImageOptions): Promise<TextImageResult> {
  const maxWidth = clamp(options.width, 320, 1600, 800)
  const fontSize = clamp(options.fontSize, 12, 48, 24)
  const padding = clamp(options.padding, 8, 80, 28)
  const lineGap = clamp(options.lineGap, 0, 40, 8)
  const text = options.text.replace(/\r\n?/g, '\n').replace(/[\u0000-\u0008\u000b-\u001f\u007f]/g, '')
  if (!text.trim()) throw new Error('请先输入要转成图片的文字')
  // 正文常见的末尾换行或空白行不应多出一行空白；中间空行仍按用户输入保留。
  const sourceLines = text.split('\n')
  while (sourceLines.length > 1 && sourceLines[sourceLines.length - 1].trim() === '') sourceLines.pop()
  await document.fonts.ready
  const canvas = document.createElement('canvas')
  const ctx = canvas.getContext('2d')
  if (!ctx) throw new Error('浏览器无法创建 Canvas')
  const font = `${fontSize}px "Microsoft YaHei UI", "SimSun", sans-serif`
  ctx.font = font
  const innerWidth = Math.max(80, maxWidth - padding * 2)
  const lines = sourceLines.flatMap((line) => wrapLine(ctx, line, innerWidth))
  const contentWidth = Math.ceil(Math.max(...lines.map((line) => ctx.measureText(line).width), 0))
  const width = Math.max(1, Math.min(maxWidth, contentWidth + padding * 2))
  const lineHeight = Math.ceil(fontSize * 1.55)
  const height = padding * 2 + lines.length * lineHeight + Math.max(0, lines.length - 1) * lineGap
  if (height > 4096) throw new Error(`图片高度 ${height}px 超过 4096px，请缩短文字或减小字号`)
  canvas.width = width
  canvas.height = height
  ctx.fillStyle = options.background
  ctx.fillRect(0, 0, canvas.width, canvas.height)
  ctx.font = font
  ctx.textBaseline = 'top'
  ctx.fillStyle = options.foreground
  lines.forEach((line, i) => {
    ctx.fillText(line, padding, padding + i * (lineHeight + lineGap))
  })
  return { dataUrl: canvas.toDataURL('image/png'), width: canvas.width, height: canvas.height }
}
