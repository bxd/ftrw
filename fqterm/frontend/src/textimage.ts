export type TextImageStyleId = 'classic' | 'aurora' | 'gold' | 'glitch'

export interface TextImagePreset {
  id: TextImageStyleId
  name: string
  description: string
  foreground: string
  background: string
  fontFamily: string
  padding: number
  lineGap: number
  lineHeight: number
  glowColor: string
  glowBlur: number
  accent: string
  accent2: string
  radius: number
  frameInset: number
  edgeBlur: number
  scanlines: boolean
  chromatic: boolean
  cardBackground: string
}

export interface TextImageOptions {
  text: string
  style?: TextImageStyleId
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
  style: 'classic',
  width: 800,
  fontSize: 24,
  foreground: '#ffffff',
  background: '#000000',
  padding: 28,
  lineGap: 8,
}

/** CSS 风格在这里翻译为 Canvas 图元，最终效果会直接写入导出的 PNG。 */
export const TEXT_IMAGE_PRESETS: readonly TextImagePreset[] = [
  {
    id: 'classic', name: '霓虹经典', description: '黑底白字 · 青色柔光',
    foreground: '#ffffff', background: '#000000',
    fontFamily: '"Microsoft YaHei UI", "SimSun", sans-serif',
    padding: 30, lineGap: 8, lineHeight: 1.6,
    glowColor: '#35e7ff', glowBlur: 12,
    accent: '#35e7ff', accent2: '#147a92', radius: 10, frameInset: 10, edgeBlur: 5,
    scanlines: false, chromatic: false,
    cardBackground: 'radial-gradient(circle at 18% 20%, rgba(53,231,255,.28), transparent 58%), linear-gradient(135deg, #02070b, #00141b)',
  },
  {
    id: 'aurora', name: '极光玻璃', description: '青紫极光 · 磨砂柔边',
    foreground: '#ecfeff', background: '#0b1026',
    fontFamily: '"Segoe UI", "Microsoft YaHei UI", sans-serif',
    padding: 44, lineGap: 10, lineHeight: 1.65,
    glowColor: '#67e8f9', glowBlur: 8,
    accent: '#67e8f9', accent2: '#a78bfa', radius: 10, frameInset: 16, edgeBlur: 6,
    scanlines: false, chromatic: false,
    cardBackground: 'radial-gradient(circle at 12% 18%, rgba(103,232,249,.65), transparent 48%), radial-gradient(circle at 88% 85%, rgba(167,139,250,.62), transparent 52%), linear-gradient(135deg, #101a38, #21133f)',
  },
  {
    id: 'gold', name: '暗金时刻', description: '香槟金光 · 柔边背景',
    foreground: '#f8e7b0', background: '#100d09',
    fontFamily: 'Georgia, "SimSun", "Songti SC", serif',
    padding: 46, lineGap: 12, lineHeight: 1.72,
    glowColor: '#d6af62', glowBlur: 6,
    accent: '#d6af62', accent2: '#6b4b1f', radius: 10, frameInset: 15, edgeBlur: 5,
    scanlines: false, chromatic: false,
    cardBackground: 'radial-gradient(ellipse at 50% 0%, rgba(214,175,98,.36), transparent 64%), linear-gradient(135deg, #171109, #070605)',
  },
  {
    id: 'glitch', name: '故障终端', description: '扫描线 · 青红残影',
    foreground: '#f4f7ff', background: '#090714',
    fontFamily: '"Cascadia Mono", "Consolas", "Microsoft YaHei UI", monospace',
    padding: 38, lineGap: 8, lineHeight: 1.58,
    glowColor: '#ff4fd8', glowBlur: 10,
    accent: '#ff4fd8', accent2: '#55f6ff', radius: 10, frameInset: 13, edgeBlur: 4,
    scanlines: true, chromatic: true,
    cardBackground: 'repeating-linear-gradient(0deg, rgba(255,255,255,.035) 0 1px, transparent 1px 4px), radial-gradient(circle at 82% 22%, rgba(85,246,255,.32), transparent 46%), radial-gradient(circle at 18% 78%, rgba(255,79,216,.34), transparent 50%), linear-gradient(135deg, #080b1b, #19091d)',
  },
]

export function textImagePreset(id: TextImageStyleId | undefined): TextImagePreset {
  return TEXT_IMAGE_PRESETS.find((preset) => preset.id === id) || TEXT_IMAGE_PRESETS[0]!
}

/** 切换方案时只覆盖该方案的颜色与排版，保留用户选好的宽度和字号。 */
export function textImagePresetOptions(id: TextImageStyleId): Pick<TextImageOptions,
  'style' | 'foreground' | 'background' | 'padding' | 'lineGap'> {
  const preset = textImagePreset(id)
  return {
    style: preset.id,
    foreground: preset.foreground,
    background: preset.background,
    padding: preset.padding,
    lineGap: preset.lineGap,
  }
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

function roundedRectPath(ctx: CanvasRenderingContext2D, x: number, y: number, width: number, height: number, radius: number) {
  const r = Math.max(0, Math.min(radius, width / 2, height / 2))
  ctx.beginPath()
  ctx.moveTo(x + r, y)
  ctx.lineTo(x + width - r, y)
  ctx.quadraticCurveTo(x + width, y, x + width, y + r)
  ctx.lineTo(x + width, y + height - r)
  ctx.quadraticCurveTo(x + width, y + height, x + width - r, y + height)
  ctx.lineTo(x + r, y + height)
  ctx.quadraticCurveTo(x, y + height, x, y + height - r)
  ctx.lineTo(x, y + r)
  ctx.quadraticCurveTo(x, y, x + r, y)
  ctx.closePath()
}

function paintBackdrop(ctx: CanvasRenderingContext2D, preset: TextImagePreset, background: string, width: number, height: number) {
  const layer = document.createElement('canvas')
  layer.width = width
  layer.height = height
  const target = layer.getContext('2d')
  if (!target) throw new Error('浏览器无法创建图片背景图层')

  target.fillStyle = background
  target.fillRect(0, 0, width, height)
  const shade = target.createLinearGradient(0, 0, width, height)
  shade.addColorStop(0, 'rgba(255,255,255,.075)')
  shade.addColorStop(.48, 'rgba(255,255,255,0)')
  shade.addColorStop(1, 'rgba(0,0,0,.48)')
  target.fillStyle = shade
  target.fillRect(0, 0, width, height)

  if (preset.id === 'aurora') {
    const cyan = target.createRadialGradient(width * .12, height * .12, 0, width * .12, height * .12, Math.max(width, height))
    cyan.addColorStop(0, 'rgba(103,232,249,.34)')
    cyan.addColorStop(1, 'rgba(103,232,249,0)')
    target.fillStyle = cyan
    target.fillRect(0, 0, width, height)
    const violet = target.createRadialGradient(width * .9, height * .9, 0, width * .9, height * .9, Math.max(width, height))
    violet.addColorStop(0, 'rgba(167,139,250,.36)')
    violet.addColorStop(1, 'rgba(167,139,250,0)')
    target.fillStyle = violet
    target.fillRect(0, 0, width, height)
  } else if (preset.id === 'gold') {
    const gold = target.createRadialGradient(width * .5, 0, 0, width * .5, 0, Math.max(width, height))
    gold.addColorStop(0, 'rgba(214,175,98,.25)')
    gold.addColorStop(1, 'rgba(214,175,98,0)')
    target.fillStyle = gold
    target.fillRect(0, 0, width, height)
  } else if (preset.id === 'glitch') {
    const neon = target.createLinearGradient(0, 0, width, height)
    neon.addColorStop(0, 'rgba(85,246,255,.12)')
    neon.addColorStop(.45, 'rgba(0,0,0,0)')
    neon.addColorStop(1, 'rgba(255,79,216,.16)')
    target.fillStyle = neon
    target.fillRect(0, 0, width, height)
  } else {
    const edge = target.createRadialGradient(0, 0, 0, 0, 0, Math.max(width, height))
    edge.addColorStop(0, 'rgba(53,231,255,.12)')
    edge.addColorStop(1, 'rgba(53,231,255,0)')
    target.fillStyle = edge
    target.fillRect(0, 0, width, height)
  }

  // 圆角遮罩只作用于背景层，文字保持清晰；再整体轻模糊，得到没有描边的柔和边缘。
  const inset = preset.frameInset
  target.globalCompositeOperation = 'destination-in'
  roundedRectPath(target, inset, inset, Math.max(1, width - inset * 2), Math.max(1, height - inset * 2), preset.radius)
  target.fillStyle = '#000'
  target.fill()
  ctx.clearRect(0, 0, width, height)
  ctx.save()
  ctx.filter = `blur(${preset.edgeBlur}px)`
  ctx.drawImage(layer, 0, 0)
  ctx.restore()
}

function paintScanlines(ctx: CanvasRenderingContext2D, preset: TextImagePreset, width: number, height: number) {
  if (!preset.scanlines) return
  const frame = preset.frameInset
  const inset = frame + 3
  ctx.save()
  roundedRectPath(ctx, frame, frame, Math.max(1, width - frame * 2), Math.max(1, height - frame * 2), preset.radius)
  ctx.clip()
  ctx.fillStyle = 'rgba(2,4,12,.2)'
  for (let y = inset; y < height - inset; y += 4) ctx.fillRect(inset, y, width - inset * 2, 1)
  const barY = Math.round(height * .38)
  ctx.fillStyle = 'rgba(85,246,255,.1)'
  ctx.fillRect(inset, barY, Math.max(1, width * .32), 2)
  ctx.fillStyle = 'rgba(255,79,216,.12)'
  ctx.fillRect(Math.round(width * .58), barY + 5, Math.max(1, width * .24), 1)
  ctx.restore()
}

/** Canvas 生成 PNG data URL；宽度是折行上限，实际画布按最长行收缩。 */
export async function renderTextImage(options: TextImageOptions): Promise<TextImageResult> {
  const maxWidth = clamp(options.width, 320, 1600, 800)
  const fontSize = clamp(options.fontSize, 12, 48, 24)
  const padding = clamp(options.padding, 8, 80, 28)
  const lineGap = clamp(options.lineGap, 0, 40, 8)
  const preset = textImagePreset(options.style)
  const frame = preset.frameInset
  const text = options.text.replace(/\r\n?/g, '\n').replace(/[\u0000-\u0008\u000b-\u001f\u007f]/g, '')
  if (!text.trim()) throw new Error('请先输入要转成图片的文字')
  // 正文常见的末尾换行或空白行不应多出一行空白；中间空行仍按用户输入保留。
  const sourceLines = text.split('\n')
  while (sourceLines.length > 1 && sourceLines[sourceLines.length - 1].trim() === '') sourceLines.pop()
  await document.fonts.ready
  const canvas = document.createElement('canvas')
  const ctx = canvas.getContext('2d')
  if (!ctx) throw new Error('浏览器无法创建 Canvas')
  const font = `${fontSize}px ${preset.fontFamily}`
  ctx.font = font
  const chrome = frame * 2
  const innerWidth = Math.max(80, maxWidth - padding * 2 - chrome)
  const lines = sourceLines.flatMap((line) => wrapLine(ctx, line, innerWidth))
  const contentWidth = Math.ceil(Math.max(...lines.map((line) => ctx.measureText(line).width), 0))
  const width = Math.max(1, Math.min(maxWidth, contentWidth + padding * 2 + chrome))
  const lineHeight = Math.ceil(fontSize * preset.lineHeight)
  const height = padding * 2 + chrome + lines.length * lineHeight + Math.max(0, lines.length - 1) * lineGap
  if (height > 4096) throw new Error(`图片高度 ${height}px 超过 4096px，请缩短文字或减小字号`)
  canvas.width = width
  canvas.height = height
  paintBackdrop(ctx, preset, options.background, width, height)
  paintScanlines(ctx, preset, width, height)

  const textX = padding + frame
  const textY = padding + frame
  ctx.font = font
  ctx.textBaseline = 'top'
  ctx.globalAlpha = 1
  if (preset.chromatic) {
    ctx.globalAlpha = .42
    ctx.fillStyle = preset.accent2
    lines.forEach((line, i) => ctx.fillText(line, textX - 2, textY + i * (lineHeight + lineGap)))
    ctx.fillStyle = preset.accent
    lines.forEach((line, i) => ctx.fillText(line, textX + 2, textY + i * (lineHeight + lineGap)))
    ctx.globalAlpha = 1
  }
  ctx.fillStyle = options.foreground
  ctx.shadowColor = preset.glowColor
  ctx.shadowBlur = preset.glowBlur
  lines.forEach((line, i) => {
    ctx.fillText(line, textX, textY + i * (lineHeight + lineGap))
  })
  ctx.shadowBlur = 0
  return { dataUrl: canvas.toDataURL('image/png'), width: canvas.width, height: canvas.height }
}
