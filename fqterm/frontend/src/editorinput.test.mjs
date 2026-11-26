import test from 'node:test'
import assert from 'node:assert/strict'
import { clipboardImageFileName, MAX_EDITOR_ATTACHMENTS, newAttachmentPaths } from './editorinput.ts'

test('附件路径按 Windows 大小写不敏感规则去重', () => {
  const existing = [{ path: 'C:\\Docs\\one.txt' }]
  assert.deepEqual(
    newAttachmentPaths(['c:\\docs\\ONE.txt', ' C:\\Docs\\two.txt ', 'C:\\Docs\\two.txt', ''], existing),
    ['C:\\Docs\\two.txt'],
  )
})

test('附件输入保留顺序并容忍空数组', () => {
  assert.deepEqual(newAttachmentPaths(['D:\\a.bin', 'E:\\b.bin'], []), ['D:\\a.bin', 'E:\\b.bin'])
  assert.deepEqual(newAttachmentPaths([], []), [])
  assert.equal(MAX_EDITOR_ATTACHMENTS, 20)
})

test('剪贴板图片临时文件名是纯 ASCII PNG', () => {
  const name = clipboardImageFileName(new Date(2026, 10, 26, 9, 8, 7, 6))
  assert.equal(name, 'clipboard-image-20261126-090807-006.png')
  assert.match(name, /^[\x20-\x7e]+$/)
})
