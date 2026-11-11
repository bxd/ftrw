# 压缩包预览 + 解压/压缩 插件（ftrw 文件管理器 Python 扩展示例）。
#
# 契约（见 plugins.go / pyrunner/runner.py）：
#   preview(path, ctx)         -> {"kind":"text","text":<文件树>,"title":..}   Ctrl+Q 触发
#   action(name, path, ctx)    -> {"message":..}                              「扩展」菜单触发
#     name ∈ {"extract","extract_to_other","compress"}
#   ctx = {"activeDir":.., "selected":[绝对路径..], "targetDir":..}
#
# zip / tar(.gz/.bz2/.xz) / 单文件 gz/bz2/xz 走纯标准库，开箱即用；
# rar / 7z 需第三方库（rarfile / py7zr），缺库时给出可辨识提示、不崩。
import os
import shutil
import zipfile
import tarfile
import gzip
import bz2
import lzma
import traceback

_TAR_SUFFIXES = (".tar.gz", ".tar.bz2", ".tar.xz", ".tgz", ".tbz2", ".txz")


# ---------- 预览：把压缩包内条目渲染成缩进文件树 ----------

def preview(path, ctx):
    title = os.path.basename(path or "")
    try:
        entries = _list_entries(path)
    except Exception:
        return {"kind": "text", "title": title,
                "text": "无法打开压缩包：\n\n" + traceback.format_exc()}
    return {"kind": "text", "title": title, "text": _render_tree(entries)}


def _list_entries(path):
    """返回 [(name, size, isdir), ...]，name 用 / 分隔。"""
    if not path or not os.path.isfile(path):
        raise RuntimeError("不是有效文件")
    low = path.lower()
    if zipfile.is_zipfile(path):
        with zipfile.ZipFile(path) as z:
            return [(i.filename, i.file_size, i.is_dir()) for i in z.infolist()]
    try:
        with tarfile.open(path, "r:*") as t:
            return [(m.name, m.size, m.isdir()) for m in t.getmembers()]
    except tarfile.ReadError:
        pass
    if low.endswith(".gz"):
        return [(_strip_comp_ext(os.path.basename(path)), os.path.getsize(path), False)]
    if low.endswith(".bz2"):
        return [(_strip_comp_ext(os.path.basename(path)), os.path.getsize(path), False)]
    if low.endswith(".xz"):
        return [(_strip_comp_ext(os.path.basename(path)), os.path.getsize(path), False)]
    if low.endswith(".rar"):
        rarfile = _optional("rarfile", "解压/预览 .rar 需要 rarfile（并在系统装有 WinRAR/UnRAR）")
        with rarfile.RarFile(path) as rf:
            return [(i.filename, i.file_size, i.isdir()) for i in rf.infolist()]
    if low.endswith(".7z"):
        py7zr = _optional("py7zr", "解压/预览 .7z 需要 py7zr 库")
        with py7zr.SevenZipFile(path) as z:
            return [(n, 0, False) for n in z.getnames()]
    raise RuntimeError("不认识的压缩包格式（支持 zip/tar/gz/bz2/xz，rar/7z 需额外库）")


def _render_tree(entries):
    tree = {}
    for name, size, isdir in entries:
        name = name.replace("\\", "/").strip("/")
        if not name:
            continue
        parts = name.split("/")
        node = tree
        for i, p in enumerate(parts):
            islast = i == len(parts) - 1
            if p not in node:
                node[p] = {"_c": {}, "_s": 0, "_d": not islast}
            cur = node[p]
            if islast:
                cur["_s"] = 0 if isdir else size
                cur["_d"] = isdir or bool(cur["_c"])
            node = cur["_c"]
    out = []

    def walk(node, prefix):
        names = sorted(node.keys(), key=lambda k: (not node[k]["_d"], k.lower()))
        for idx, n in enumerate(names):
            last = idx == len(names) - 1
            cur = node[n]
            label = n + ("/" if cur["_d"] else "")
            if not cur["_d"] and cur["_s"]:
                label += "  (%s)" % _fmt_size(cur["_s"])
            out.append(prefix + ("└── " if last else "├── ") + label)
            if cur["_c"]:
                walk(cur["_c"], prefix + ("    " if last else "│   "))

    walk(tree, "")
    return "\n".join(out) if out else "（空压缩包）"


# ---------- 动作：解压 / 压缩 ----------

def action(name, path, ctx):
    if name == "extract":
        return _extract(path, ctx.get("activeDir") or _dirname_or(path, "."))
    if name == "extract_to_other":
        return _extract(path, ctx.get("targetDir") or ctx.get("activeDir") or _dirname_or(path, "."))
    if name == "compress":
        return _compress(ctx)
    return {"message": "未知动作：%s" % name}


def _extract(path, dest_dir):
    if not path or not os.path.isfile(path):
        return {"message": "请把光标停在压缩包文件上，再选「解压」"}
    dest = _unique(os.path.join(dest_dir, _archive_stem(os.path.basename(path))))
    try:
        os.makedirs(dest, exist_ok=True)
        n = _extract_all(path, dest)
    except Exception:
        return {"message": "解压失败：\n" + traceback.format_exc()}
    return {"message": "已解压 %d 项 → %s" % (n, dest)}


def _extract_all(path, dest):
    low = path.lower()
    if zipfile.is_zipfile(path):
        with zipfile.ZipFile(path) as z:
            _safe_extract_zip(z, dest)
            return len(z.namelist())
    try:
        with tarfile.open(path, "r:*") as t:
            _safe_extract_tar(t, dest)
            return len(t.getmembers())
    except tarfile.ReadError:
        pass
    if low.endswith(".gz"):
        return _extract_single(path, dest, gzip.open)
    if low.endswith(".bz2"):
        return _extract_single(path, dest, bz2.open)
    if low.endswith(".xz"):
        return _extract_single(path, dest, lzma.open)
    if low.endswith(".rar"):
        rarfile = _optional("rarfile", "解压 .rar 需要 rarfile（并在系统装有 WinRAR/UnRAR）")
        with rarfile.RarFile(path) as rf:
            rf.extractall(dest)
            return len(rf.namelist())
    if low.endswith(".7z"):
        py7zr = _optional("py7zr", "解压 .7z 需要 py7zr 库")
        with py7zr.SevenZipFile(path) as z:
            z.extractall(dest)
            return len(z.getnames())
    raise RuntimeError("不认识的压缩包格式")


def _safe_extract_zip(z, dest):
    base = os.path.abspath(dest)
    for info in z.infolist():
        target = os.path.abspath(os.path.join(dest, info.filename.replace("\\", "/")))
        if target != base and not target.startswith(base + os.sep):
            continue  # 防 zip-slip 目录穿越
        if info.is_dir():
            os.makedirs(target, exist_ok=True)
        else:
            os.makedirs(os.path.dirname(target), exist_ok=True)
            with z.open(info) as src, open(target, "wb") as dst:
                shutil.copyfileobj(src, dst)


def _safe_extract_tar(t, dest):
    base = os.path.abspath(dest)
    for m in t.getmembers():
        target = os.path.abspath(os.path.join(dest, m.name.replace("\\", "/")))
        if target != base and not target.startswith(base + os.sep):
            continue
        if m.isdir():
            os.makedirs(target, exist_ok=True)
        elif m.isreg():
            os.makedirs(os.path.dirname(target), exist_ok=True)
            f = t.extractfile(m)
            if f is not None:
                with f, open(target, "wb") as dst:
                    shutil.copyfileobj(f, dst)


def _extract_single(path, dest, opener):
    name = _strip_comp_ext(os.path.basename(path)) or "output"
    target = os.path.join(dest, name)
    with opener(path, "rb") as src, open(target, "wb") as dst:
        shutil.copyfileobj(src, dst)
    return 1


def _compress(ctx):
    sel = [p for p in (ctx.get("selected") or []) if p and os.path.exists(p)]
    if not sel:
        return {"message": "未选中任何文件/目录（先用 Insert 或 Ctrl+点击 选择，再压缩）"}
    active = ctx.get("activeDir") or os.path.dirname(sel[0])
    stem = os.path.splitext(os.path.basename(sel[0]))[0] if len(sel) == 1 else "新建压缩包"
    out = _unique(os.path.join(active, stem + ".zip"))
    count = 0
    try:
        with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
            for p in sel:
                if os.path.isdir(p):
                    for root, _dirs, files in os.walk(p):
                        for fn in files:
                            fp = os.path.join(root, fn)
                            z.write(fp, os.path.relpath(fp, os.path.dirname(p)))
                            count += 1
                else:
                    z.write(p, os.path.basename(p))
                    count += 1
    except Exception:
        return {"message": "压缩失败：\n" + traceback.format_exc()}
    return {"message": "已压缩 %d 个文件 → %s" % (count, out)}


# ---------- 小工具 ----------

def _optional(mod, hint):
    try:
        return __import__(mod)
    except ImportError:
        raise RuntimeError("缺少库 %s：%s（可在插件目录写 requirements.txt 后重试）" % (mod, hint))


def _archive_stem(fn):
    low = fn.lower()
    for suf in _TAR_SUFFIXES:
        if low.endswith(suf):
            return fn[:-len(suf)]
    return os.path.splitext(fn)[0]


def _strip_comp_ext(fn):
    for suf in (".gz", ".bz2", ".xz"):
        if fn.lower().endswith(suf):
            return fn[:-len(suf)]
    return fn


def _dirname_or(path, default):
    d = os.path.dirname(path or "")
    return d or default


def _unique(p):
    if not os.path.exists(p):
        return p
    base, ext = os.path.splitext(p)
    i = 1
    while os.path.exists("%s(%d)%s" % (base, i, ext)):
        i += 1
    return "%s(%d)%s" % (base, i, ext)


def _fmt_size(n):
    n = float(n)
    for u in ("B", "K", "M", "G", "T"):
        if n < 1024:
            return ("%d%s" % (n, u)) if u == "B" else ("%.1f%s" % (n, u))
        n /= 1024
    return "%.1fP" % n
