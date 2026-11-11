# 示例 ftrw 预览插件：把 .docx 抽成纯文本预览。只用 Python 标准库（zipfile + xml），
# 无需 requirements.txt，装好 Python 即可跑。丢进 %APPDATA%\FTRW\plugins\<任意目录名>\ 就生效。
#
# 契约：def preview(path, ctx) -> {"kind","text"|"html"|"path","title"}
#   - kind: image / audio / video / pdf / html / text
#   - 内联文本/HTML 直接回 text / html；要回文件就把产物写到本目录 _work/ 并回 {"path": 绝对路径}，
#     Go 会把它签成可渲染 URL（像这里其实直接回 text 更省事）。
import os
import zipfile
from xml.etree import ElementTree as ET

W = "{http://schemas.openxmlformats.org/wordprocessingml/2006/main}"


def preview(path, ctx):
    name = os.path.basename(path)
    try:
        with zipfile.ZipFile(path) as z:
            xml = z.read("word/document.xml")
        root = ET.fromstring(xml)
        lines = []
        for p in root.iter(W + "p"):
            lines.append("".join(t.text or "" for t in p.iter(W + "t")))
        text = "\n".join(lines).strip()
        return {"kind": "text", "text": text or "（空文档）", "title": name}
    except Exception as e:  # 解析失败也要优雅回文本，别让预览整片空白
        return {"kind": "text", "text": "docx 解析失败：%s" % e, "title": name}
