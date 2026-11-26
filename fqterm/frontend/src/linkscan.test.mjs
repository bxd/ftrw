import test from 'node:test'
import assert from 'node:assert/strict'
import { urlSpanAt } from './linkscan.ts'

test('链接悬停命中返回完整 URL 和终端列区间', () => {
  const text = '见图 https://example.com/a.png, 然后'
  const start = text.indexOf('https://')
  const url = 'https://example.com/a.png'
  assert.deepEqual(urlSpanAt(text, start), { url, start, end: start + url.length })
  assert.equal(urlSpanAt(text, start + url.length - 1)?.url, url)
  assert.equal(urlSpanAt(text, start - 1), null)
  assert.equal(urlSpanAt(text, start + url.length), null)
})

test('链接悬停使用与点击相同的结尾标点清理规则', () => {
  const text = '附件(https://example.com/att.php?ABC.png)，下一页'
  const start = text.indexOf('https://')
  assert.equal(urlSpanAt(text, start)?.url, 'https://example.com/att.php?ABC.png')
  assert.equal(urlSpanAt(text, text.indexOf('，')), null)

  const balanced = '百科 https://example.com/Foo_(bar) 完'
  const at = balanced.indexOf('https://')
  assert.equal(urlSpanAt(balanced, balanced.indexOf(')'))?.url, 'https://example.com/Foo_(bar)')
  assert.equal(urlSpanAt(balanced, at - 1), null)
})

test('同一行多个链接按鼠标所在列选择，不误取相邻地址', () => {
  const text = 'https://a.example/one.jpg https://b.example/two.mp3'
  const first = text.indexOf('https://')
  const second = text.indexOf('https://', first + 1)
  assert.equal(urlSpanAt(text, first + 8)?.url, 'https://a.example/one.jpg')
  assert.equal(urlSpanAt(text, second + 8)?.url, 'https://b.example/two.mp3')
  assert.equal(urlSpanAt(text, second - 1), null)
})
