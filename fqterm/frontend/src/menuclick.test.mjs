import test from 'node:test'
import assert from 'node:assert/strict'
import { detectPageState, hitTest, itemsInRow, parsePostOptionsFields, rowColumnText, rowText, visibleText } from './menuclick.ts'
import { extractURLSpans } from './linkscan.ts'

function screen(lines) {
  const rows = lines.length
  const cells = lines.map((line) => {
    const row = []
    for (const ch of line) {
      const width = /[\p{Script=Han}\p{Script=Hiragana}\p{Script=Katakana}\p{Script=Hangul}]/u.test(ch) ? 2 : 1
      row.push(ch, ...Array(width - 1).fill(''))
    }
    return row
  })
  const cols = Math.max(80, ...cells.map((row) => row.length))
  return {
    rows,
    cols,
    caretRow: 0,
    rowCells(row) {
      return [...cells[row], ...Array(cols - cells[row].length).fill(' ')]
    },
  }
}

function state(lines) {
  return detectPageState(screen(lines))
}

test('语义文本保留连续汉字，显示列文本仍按终端列对齐', () => {
  const line = '发表文章于 Test 讨论区'
  const s = screen([line])
  assert.equal(rowText(s, 0).trimEnd(), line)
  assert.equal(rowColumnText(s, 0).length, s.cols)
  assert.equal(rowColumnText(s, 0).indexOf('Test'), 11)
  assert.equal(visibleText(s), line)
})

test('中文菜单标签的点击区间仍按显示列计算', () => {
  const s = screen([
    '1) 分类讨论区     2) 个人工具箱',
    '3) 精品文章       4) 站务管理',
  ])
  assert.equal(detectPageState(s), 'menu')
  const items = itemsInRow(s, 0)
  assert.deepEqual(items.map((item) => item.token), ['1', '2'])
  assert.equal(items[1].start, s.rowCells(0).indexOf('2'))
  const hit = hitTest(s, 'menu', 0, items[1].start + 4)
  assert.equal(hit?.label, '菜单项 2')
})

test('中文前缀后的 URL 区间仍按显示列计算', () => {
  const s = screen(['水木 https://example.com/a.png'])
  const url = 'https://example.com/a.png'
  const start = s.rowCells(0).indexOf('h')
  assert.deepEqual(extractURLSpans(rowColumnText(s, 0)), [[start, start + url.length]])
})

test('水木普通发文设置页无需匿名或转信即可识别', () => {
  assert.equal(state([
    '发表文章于 Test 讨论区',
    '使用标题: 自动编辑器回归测试',
    '使用第 1 个签名档',
    'b回复到信箱，T改标题，Q放弃，Enter继续:',
  ]), 'post-options')
})

test('水木发文上下文提取真实标题与版名', () => {
  assert.deepEqual(parsePostOptionsFields([
    '发表文章于 Test 讨论区',
    '使用标题: 自动编辑器回归测试',
    '使用第 1 个签名档',
    'b回复到信箱，T改标题，Q放弃，Enter继续:',
  ].join('\n')), { title: '自动编辑器回归测试', board: 'Test' })
})

test('旧版发文上下文格式保持兼容', () => {
  assert.deepEqual(parsePostOptionsFields([
    '讨论区: Legacy',
    '标题: 旧版标题',
    '签名档: 1',
    '匿名',
    '接受所有设定',
  ].join('\n')), { title: '旧版标题', board: 'Legacy' })
})

test('水木随机签名档发文页可识别', () => {
  assert.equal(state([
    '发表文章于 Test 讨论区',
    '使用标题: 自动编辑器回归测试',
    '使用随机签名档',
    'b回复到信箱，T改标题，Q放弃，Enter继续:',
  ]), 'post-options')
})

test('水木匿名版发文页仍可识别', () => {
  assert.equal(state([
    '发表文章于 Anonymous 讨论区  使用匿名',
    '使用标题: 自动编辑器回归测试',
    '使用第 2 个签名档',
    'M匿名，b回复到信箱，T改标题，Q放弃，Enter继续:',
  ]), 'post-options')
})

test('初始标题输入页不会提前触发自动接管', () => {
  assert.equal(state([
    '发表文章于 Test 讨论区',
    '使用标题: [正在设定主题]',
    '使用第 1 个签名档',
    '标题:',
  ]), 'unknown')
})

test('阅读正文提到标题和签名档不会误判', () => {
  assert.equal(state([
    '发信人: tester',
    '标 题: 签名档和标题设置说明',
    '发信站: 水木社区',
    '正文：匿名、转信、Enter接受都不是发文设置页。',
  ]), 'reading')
})

test('水木编辑器插入状态可识别', () => {
  assert.equal(state([
    '正文第一行',
    'Ctrl-Q 求救    状态 [插入][1,5][ ][ ]     时间',
  ]), 'editing')
})

test('水木编辑器替换状态可识别', () => {
  assert.equal(state([
    '正文第一行',
    'Ctrl-Q 求救    状态 [替换][1,5][ ][ ]     时间',
  ]), 'editing')
})

test('旧版插入与覆盖状态格式保持兼容', () => {
  assert.equal(state([
    '正文第一行',
    'Ctrl-Q 帮助  插入|OVER',
  ]), 'editing')
})

test('缺少编辑模式时不会误判为编辑器', () => {
  assert.equal(state([
    'Ctrl-Q 求救',
    '正文：这里没有编辑器状态行',
  ]), 'unknown')
})

test('缺少帮助提示时不会误判为编辑器', () => {
  assert.equal(state([
    '状态 [插入] 是编辑器的显示方式',
    '标题: 状态 [替换]',
  ]), 'unknown')
})

test('旧版发文设置页保持兼容', () => {
  assert.equal(state([
    '标题: 自动编辑器回归测试',
    '签名档: 1',
    '匿名',
    '接受所有设定',
  ]), 'post-options')
})

test('发表确认页识别不回退', () => {
  assert.equal(state([
    '确认发表文章?',
    '(L)站内  (F)转信  [L]:',
  ]), 'post-confirm')
})
