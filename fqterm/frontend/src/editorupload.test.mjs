import test from 'node:test'
import assert from 'node:assert/strict'
import { EDITOR_ENTER_TRIGGER, EDITOR_UPLOAD_TRIGGER, markEditorDraftSentAndDelete, uploadEditorAttachmentsSequentially } from './editorupload.ts'

function fixture(paths = ['a.png', 'b.png', 'c.png']) {
  const calls = []
  const uploaded = []
  const tab = {
    id: 'session',
    composeContext: () => ({ state: 'post-options' }),
  }
  const io = {
    async clearStagedUpload(id) { calls.push(['clear', id]) },
    async stageUpload(id, files) { calls.push(['stage', id, [...files]]) },
    async transferOne(target, path) {
      calls.push(['transfer', target.id, path])
    },
  }
  return { tab, io, calls, uploaded, paths }
}

test('水木附件命令必须用回车提交 getdata，整批结束后再单独发一次回车', () => {
  assert.equal(EDITOR_UPLOAD_TRIGGER, 'U\r')
  assert.equal(EDITOR_ENTER_TRIGGER, '\r')
})

test('水木附件逐文件暂存、逐次触发 U，不依赖屏幕重绘识别', async () => {
  const f = fixture()
  await uploadEditorAttachmentsSequentially(f.tab, f.paths, f.io, (path) => f.uploaded.push(path))
  assert.deepEqual(f.uploaded, f.paths)
  assert.deepEqual(f.calls, [
    ['clear', 'session'],
    ['stage', 'session', ['a.png']],
    ['transfer', 'session', 'a.png'],
    ['stage', 'session', ['b.png']],
    ['transfer', 'session', 'b.png'],
    ['stage', 'session', ['c.png']],
    ['transfer', 'session', 'c.png'],
  ])
})

test('首份上传后屏幕识别暂时不可用时也继续下一份附件', async () => {
  const f = fixture(['a.png', 'b.png'])
  const original = f.io.transferOne
  f.io.transferOne = async (tab, path) => {
    await original(tab, path)
    f.tab.composeContext = () => null
  }
  await uploadEditorAttachmentsSequentially(f.tab, f.paths, f.io, (path) => f.uploaded.push(path))
  assert.deepEqual(f.uploaded, f.paths)
  assert.deepEqual(f.calls.filter(([kind]) => kind === 'transfer'), [
    ['transfer', 'session', 'a.png'],
    ['transfer', 'session', 'b.png'],
  ])
})

test('第二份传输失败时保留首份完成状态并清理暂存', async () => {
  const f = fixture()
  f.io.transferOne = async (_tab, path) => {
    if (path === 'b.png') throw new Error('second failed')
  }
  await assert.rejects(
    uploadEditorAttachmentsSequentially(f.tab, f.paths, f.io, (path) => f.uploaded.push(path)),
    /second failed/,
  )
  assert.deepEqual(f.uploaded, ['a.png'])
  assert.deepEqual(f.calls.filter(([kind]) => kind === 'stage'), [
    ['stage', 'session', ['a.png']],
    ['stage', 'session', ['b.png']],
  ])
  assert.equal(f.calls.filter(([kind]) => kind === 'clear').length, 2)
})

test('整批附件完成后不发送额外回车，最终回车由调用方统一发送', async () => {
  const f = fixture(['a.png', 'b.png'])
  await uploadEditorAttachmentsSequentially(f.tab, f.paths, f.io, (path) => f.uploaded.push(path))
  assert.deepEqual(f.uploaded, f.paths)
  assert.equal(f.calls.filter(([kind]) => kind === 'transfer').length, 2)
  assert.equal(f.calls.some(([kind, , value]) => kind === 'transfer' && value === EDITOR_ENTER_TRIGGER), false)
})

test('回传成功先标记 sent，再删除对应草稿', async () => {
  const calls = []
  const sent = []
  const draft = { id: 'draft-1', revision: 2, transferState: 'sending', synced: false, updatedAt: 1 }
  const result = await markEditorDraftSentAndDelete(draft, {
    async save(value) { calls.push(['save', value]); return { ...value, revision: value.revision + 1 } },
    async delete(id) { calls.push(['delete', id]) },
  }, (value) => sent.push(value))
  assert.deepEqual(calls.map(([kind]) => kind), ['save', 'delete'])
  assert.equal(calls[0][1].transferState, 'sent')
  assert.equal(calls[0][1].synced, true)
  assert.equal(calls[1][1], 'draft-1')
  assert.equal(sent.length, 1)
  assert.equal(result.revision, 3)
})

test('sent 标记保存失败时不删除草稿', async () => {
  let deleted = false
  await assert.rejects(markEditorDraftSentAndDelete({ id: 'draft-1' }, {
    async save() { throw new Error('save failed') },
    async delete() { deleted = true },
  }), /save failed/)
  assert.equal(deleted, false)
})

test('删除失败时保留 sent 快照并向上报告', async () => {
  let saved
  await assert.rejects(markEditorDraftSentAndDelete({ id: 'draft-1' }, {
    async save(value) { saved = { ...value, revision: 1 }; return saved },
    async delete() { throw new Error('delete failed') },
  }, (value) => { saved = value }), /delete failed/)
  assert.equal(saved.transferState, 'sent')
  assert.equal(saved.synced, true)
})

test('附件路径按 Windows 大小写不敏感规则去重', async () => {
  const f = fixture(['C:\\A.png', 'c:\\a.PNG', 'B.png'])
  await uploadEditorAttachmentsSequentially(f.tab, f.paths, f.io, (path) => f.uploaded.push(path))
  assert.deepEqual(f.uploaded, ['C:\\A.png', 'B.png'])
})
