import test from 'node:test'
import assert from 'node:assert/strict'
import { renderTextImage, TEXT_IMAGE_DEFAULTS, textImageFileName } from './textimage.ts'

const baseOptions = {
  text: '短文字',
  ...TEXT_IMAGE_DEFAULTS,
}

function mockCanvas(t, measure = (text) => Array.from(text).length * 10) {
  const previous = Object.getOwnPropertyDescriptor(globalThis, 'document')
  const drawn = []
  const painted = []
  const canvas = {
    width: 0,
    height: 0,
    getContext(kind) {
      assert.equal(kind, '2d')
      return {
        font: '',
        fillStyle: '',
        textBaseline: '',
        measureText(text) { return { width: measure(text) } },
        fillRect() { painted.push({ fillStyle: this.fillStyle, text: null }) },
        fillText(text) { drawn.push(text); painted.push({ fillStyle: this.fillStyle, text }) },
      }
    },
    toDataURL(type) {
      assert.equal(type, 'image/png')
      return 'data:image/png;base64,mock'
    },
  }
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: {
      fonts: { ready: Promise.resolve() },
      createElement(tag) {
        assert.equal(tag, 'canvas')
        return canvas
      },
    },
  })
  t.after(() => {
    if (previous) Object.defineProperty(globalThis, 'document', previous)
    else delete globalThis.document
  })
  return { canvas, drawn, painted }
}

test('文字图片默认黑底白字', async (t) => {
  const { painted } = mockCanvas(t)
  assert.equal(TEXT_IMAGE_DEFAULTS.foreground, '#ffffff')
  assert.equal(TEXT_IMAGE_DEFAULTS.background, '#000000')
  await renderTextImage(baseOptions)
  assert.deepEqual(painted, [
    { fillStyle: '#000000', text: null },
    { fillStyle: '#ffffff', text: '短文字' },
  ])
})

test('文字图片文件名只含 ASCII，并按本地时间补齐日期时间', () => {
  const name = textImageFileName(new Date(2026, 0, 2, 3, 4, 5))
  assert.equal(name, 'text-image-20260102-030405')
  assert.match(name, /^[A-Za-z0-9-]+$/)
})

test('短文字图片按最长行收缩宽度，不再保留配置宽度右侧空白', async (t) => {
  const { canvas, drawn } = mockCanvas(t)
  const result = await renderTextImage({ ...baseOptions, text: '短' })
  assert.equal(result.width, 66)
  assert.equal(result.width, canvas.width)
  assert.ok(result.width < baseOptions.width)
  assert.equal(result.height, 94)
  assert.deepEqual(drawn, ['短'])
})

test('多行图片宽度取最长行，高度只包含实际行数', async (t) => {
  mockCanvas(t)
  const result = await renderTextImage({ ...baseOptions, text: '一\n四字文本' })
  assert.equal(result.width, 96)
  assert.equal(result.height, 140)
})

test('末尾换行和纯空白行不撑高图片，中间空行仍保留', async (t) => {
  const { drawn } = mockCanvas(t)
  const result = await renderTextImage({ ...baseOptions, text: '第一行\n\n第二行\n\n  \n' })
  assert.equal(result.height, 186)
  assert.deepEqual(drawn, ['第一行', '', '第二行'])
})

test('长文字仍按最大宽度折行，并按折行后的行数增加高度', async (t) => {
  const { drawn } = mockCanvas(t)
  const text = 'a'.repeat(30)
  const result = await renderTextImage({ ...baseOptions, text, width: 320 })
  assert.equal(result.width, 316)
  assert.deepEqual(drawn, ['a'.repeat(26), 'a'.repeat(4)])
  assert.equal(result.height, 140)
})

test('代理对字符按码点折行，不会被拆成半个字符', async (t) => {
  const { drawn } = mockCanvas(t)
  const text = '😀'.repeat(28)
  const result = await renderTextImage({ ...baseOptions, text, width: 320 })
  assert.equal(result.width, 316)
  assert.deepEqual(drawn, ['😀'.repeat(26), '😀'.repeat(2)])
})

test('空白文字不能生成图片', async () => {
  await assert.rejects(renderTextImage({ ...baseOptions, text: ' \n\t ' }), /请先输入/)
})
