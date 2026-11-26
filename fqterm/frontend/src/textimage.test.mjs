import test from 'node:test'
import assert from 'node:assert/strict'
import { renderTextImage, TEXT_IMAGE_DEFAULTS, TEXT_IMAGE_PRESETS, textImageFileName, textImagePreset, textImagePresetOptions } from './textimage.ts'

const baseOptions = {
  text: '短文字',
  ...TEXT_IMAGE_DEFAULTS,
}

function styleLabel(style) {
  return style && typeof style === 'object' ? `${style.kind}-gradient` : style
}

function mockCanvas(t, measure = (text) => Array.from(text).length * 10) {
  const previous = Object.getOwnPropertyDescriptor(globalThis, 'document')
  const drawn = []
  const painted = []
  const fonts = []
  const strokes = []
  const pathStrokes = []
  const shadows = []
  const gradients = []
  const rects = []
  const alphas = []
  const drawImages = []
  const clears = []
  const composites = []
  let clips = 0
  let fills = 0
  let created = 0
  const gradient = (kind, coords) => {
    const value = {
      kind,
      coords,
      stops: [],
      addColorStop(offset, color) { this.stops.push({ offset, color }) },
    }
    gradients.push(value)
    return value
  }
  const context = () => ({
    font: '',
    fillStyle: '',
    strokeStyle: '',
    lineWidth: 1,
    shadowColor: '',
    shadowBlur: 0,
    shadowOffsetX: 0,
    shadowOffsetY: 0,
    textBaseline: '',
    globalAlpha: 1,
    globalCompositeOperation: 'source-over',
    filter: 'none',
    createLinearGradient(...coords) { return gradient('linear', coords) },
    createRadialGradient(...coords) { return gradient('radial', coords) },
    beginPath() {},
    closePath() {},
    moveTo() {},
    lineTo() {},
    quadraticCurveTo() {},
    save() {},
    restore() {},
    clip() { clips++ },
    clearRect(...args) { clears.push(args) },
    drawImage(source, x, y) {
      drawImages.push({ width: source.width, height: source.height, x, y, filter: this.filter })
    },
    fill() {
      fills++
      composites.push(this.globalCompositeOperation)
      painted.push({ fillStyle: styleLabel(this.fillStyle), text: null })
    },
    stroke() {
      pathStrokes.push({ color: this.strokeStyle, lineWidth: this.lineWidth })
    },
    measureText(text) { return { width: measure(text) } },
    fillRect(...args) {
      rects.push(args)
      painted.push({ fillStyle: styleLabel(this.fillStyle), text: null })
    },
    strokeRect(...args) {
      strokes.push({ color: this.strokeStyle, lineWidth: this.lineWidth, args })
    },
    fillText(text, x, y) {
      drawn.push(text)
      fonts.push(this.font)
      alphas.push(this.globalAlpha)
      shadows.push({ color: this.shadowColor, blur: this.shadowBlur })
      painted.push({ fillStyle: styleLabel(this.fillStyle), text })
    },
  })
  const makeCanvas = () => ({
    width: 0,
    height: 0,
    getContext(kind) {
      assert.equal(kind, '2d')
      return context()
    },
    toDataURL(type) {
      assert.equal(type, 'image/png')
      return 'data:image/png;base64,mock'
    },
  })
  const canvas = makeCanvas()
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: {
      fonts: { ready: Promise.resolve() },
      createElement(tag) {
        assert.equal(tag, 'canvas')
        created++
        return created === 1 ? canvas : makeCanvas()
      },
    },
  })
  t.after(() => {
    if (previous) Object.defineProperty(globalThis, 'document', previous)
    else delete globalThis.document
  })
  return {
    canvas, drawn, painted, fonts, strokes, pathStrokes, shadows, gradients, rects, alphas,
    drawImages, clears, composites,
    canvases: () => created,
    clips: () => clips,
    fills: () => fills,
  }
}

test('默认仍是黑底白字，并使用无描边的小圆角柔边背景', async (t) => {
  const { painted, pathStrokes, strokes, shadows, gradients, drawImages, clears, composites } = mockCanvas(t)
  assert.equal(TEXT_IMAGE_DEFAULTS.style, 'classic')
  assert.equal(TEXT_IMAGE_DEFAULTS.foreground, '#ffffff')
  assert.equal(TEXT_IMAGE_DEFAULTS.background, '#000000')
  const result = await renderTextImage(baseOptions)
  assert.equal(result.width, 106)
  assert.equal(result.height, 115)
  assert.equal(painted[0].fillStyle, '#000000')
  assert.equal(painted.at(-1).fillStyle, '#ffffff')
  assert.equal(painted.at(-1).text, '短文字')
  assert.ok(gradients.some((item) => item.kind === 'linear'))
  assert.ok(gradients.some((item) => item.kind === 'radial'))
  assert.equal(pathStrokes.length, 0)
  assert.equal(strokes.length, 0)
  assert.equal(clears.length, 1)
  assert.ok(composites.includes('destination-in'))
  assert.deepEqual(drawImages, [{ width: 106, height: 115, x: 0, y: 0, filter: 'blur(5px)' }])
  assert.deepEqual(shadows.at(-1), { color: '#35e7ff', blur: 12 })
})

test('提供四套带 CSS 渐变预览且可回退的炫酷方案', () => {
  assert.deepEqual(TEXT_IMAGE_PRESETS.map((preset) => preset.id), ['classic', 'aurora', 'gold', 'glitch'])
  assert.equal(new Set(TEXT_IMAGE_PRESETS.map((preset) => preset.name)).size, 4)
  assert.ok(TEXT_IMAGE_PRESETS.every((preset) => preset.cardBackground.includes('gradient')))
  assert.ok(TEXT_IMAGE_PRESETS.every((preset) => preset.radius === 10 && preset.edgeBlur > 0))
  assert.equal(textImagePreset(undefined).id, 'classic')
  assert.equal(textImagePreset('unknown').id, 'classic')
})

test('切换方案只覆盖颜色和排版，保留用户宽度与字号', () => {
  const current = { ...baseOptions, width: 1234, fontSize: 31 }
  const next = { ...current, ...textImagePresetOptions('aurora') }
  assert.equal(next.width, 1234)
  assert.equal(next.fontSize, 31)
  assert.equal(next.style, 'aurora')
  assert.equal(next.foreground, '#ecfeff')
  assert.equal(next.background, '#0b1026')
  assert.equal(next.padding, 44)
  assert.equal(next.lineGap, 10)
})

test('极光玻璃绘制双色径向极光、磨砂面板和圆角柔边', async (t) => {
  const { fonts, gradients, pathStrokes, drawImages } = mockCanvas(t)
  const result = await renderTextImage({ ...baseOptions, ...textImagePresetOptions('aurora') })
  assert.equal(result.width, 150)
  assert.equal(result.height, 160)
  assert.match(fonts[0], /Segoe UI/)
  assert.equal(gradients.filter((item) => item.kind === 'radial').length, 2)
  assert.equal(pathStrokes.length, 0)
  assert.equal(drawImages[0].filter, 'blur(6px)')
})

test('暗金时刻绘制金色径向光和无描边柔边背景', async (t) => {
  const { fonts, gradients, pathStrokes, drawImages } = mockCanvas(t)
  await renderTextImage({ ...baseOptions, ...textImagePresetOptions('gold') })
  assert.match(fonts[0], /Georgia/)
  assert.equal(gradients.filter((item) => item.kind === 'radial').length, 1)
  assert.equal(pathStrokes.length, 0)
  assert.equal(drawImages[0].filter, 'blur(5px)')
})

test('故障终端绘制扫描线、青红残影和主文字三层', async (t) => {
  const { drawn, fonts, alphas, rects, pathStrokes, clips } = mockCanvas(t)
  await renderTextImage({ ...baseOptions, ...textImagePresetOptions('glitch') })
  assert.match(fonts[0], /Cascadia Mono/)
  assert.deepEqual(drawn, ['短文字', '短文字', '短文字'])
  assert.deepEqual(alphas, [.42, .42, 1])
  assert.ok(rects.length > 10)
  assert.equal(pathStrokes.length, 0)
  assert.equal(clips(), 1)
})

test('所有方案都移除边框线并输出模糊圆角背景', async (t) => {
  for (const preset of TEXT_IMAGE_PRESETS) {
    await t.test(preset.id, async (t) => {
      const { strokes, pathStrokes, drawImages, composites } = mockCanvas(t)
      await renderTextImage({ ...baseOptions, ...textImagePresetOptions(preset.id) })
      assert.equal(strokes.length, 0)
      assert.equal(pathStrokes.length, 0)
      assert.ok(composites.includes('destination-in'))
      assert.equal(drawImages.length, 1)
      assert.match(drawImages[0].filter, /^blur\(\d+px\)$/)
    })
  }
})

test('选定方案后仍允许自定义前景与背景颜色', async (t) => {
  const { painted } = mockCanvas(t)
  await renderTextImage({
    ...baseOptions,
    ...textImagePresetOptions('aurora'),
    foreground: '#123456',
    background: '#abcdef',
  })
  assert.equal(painted[0].fillStyle, '#abcdef')
  assert.equal(painted.at(-1).fillStyle, '#123456')
  assert.equal(painted.at(-1).text, '短文字')
})

test('文字图片文件名只含 ASCII，并按本地时间补齐日期时间', () => {
  const name = textImageFileName(new Date(2026, 0, 2, 3, 4, 5))
  assert.equal(name, 'text-image-20260102-030405')
  assert.match(name, /^[A-Za-z0-9-]+$/)
})

test('短文字图片按最长行收缩宽度，不再保留配置宽度右侧空白', async (t) => {
  const { canvas, drawn } = mockCanvas(t)
  const result = await renderTextImage({ ...baseOptions, text: '短' })
  assert.equal(result.width, 86)
  assert.equal(result.width, canvas.width)
  assert.ok(result.width < baseOptions.width)
  assert.equal(result.height, 115)
  assert.deepEqual(drawn, ['短'])
})

test('多行图片宽度取最长行，高度只包含实际行数', async (t) => {
  mockCanvas(t)
  const result = await renderTextImage({ ...baseOptions, text: '一\n四字文本' })
  assert.equal(result.width, 116)
  assert.equal(result.height, 162)
})

test('末尾换行和纯空白行不撑高图片，中间空行仍保留', async (t) => {
  const { drawn } = mockCanvas(t)
  const result = await renderTextImage({ ...baseOptions, text: '第一行\n\n第二行\n\n  \n' })
  assert.equal(result.height, 209)
  assert.deepEqual(drawn, ['第一行', '', '第二行'])
})

test('长文字仍按最大宽度折行，并按折行后的行数增加高度', async (t) => {
  const { drawn } = mockCanvas(t)
  const text = 'a'.repeat(30)
  const result = await renderTextImage({ ...baseOptions, text, width: 320 })
  assert.equal(result.width, 316)
  assert.deepEqual(drawn, ['a'.repeat(24), 'a'.repeat(6)])
  assert.equal(result.height, 162)
})

test('代理对字符按码点折行，不会被拆成半个字符', async (t) => {
  const { drawn } = mockCanvas(t)
  const text = '😀'.repeat(28)
  const result = await renderTextImage({ ...baseOptions, text, width: 320 })
  assert.equal(result.width, 316)
  assert.deepEqual(drawn, ['😀'.repeat(24), '😀'.repeat(4)])
})

test('空白文字不能生成图片', async () => {
  await assert.rejects(renderTextImage({ ...baseOptions, text: ' \n\t ' }), /请先输入/)
})
