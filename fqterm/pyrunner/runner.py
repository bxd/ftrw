# ftrw 文件管理器 Python 插件的通用宿主（纯标准库）。
#
# 用法：  python -u runner.py <plugin_dir> <entry_file> <preview_func> [action_func]
# 协议：  stdin 每行一条 JSON 请求，stdout 每行一条 JSON 响应（行分隔 JSON-RPC）。
#   预览  {"jsonrpc":"2.0","id":N,"method":"preview","params":{"path":..,"ctx":{..}}}
#         -> {"...","result":<artifact>}          artifact = {"kind","path"|"html"|"text","title"}
#   动作  {"jsonrpc":"2.0","id":N,"method":"action","params":{"name":..,"path":..,"ctx":{..}}}
#         -> {"...","result":{"message":..}}       解压 / 压缩等有副作用的操作走这条
#   失败  {"jsonrpc":"2.0","id":N,"error":{"message":..}}
#
# 一个进程只服务一个插件（因为每插件各自 venv，解释器不同）。插件 import 失败不致命，
# 每次请求都回一条带 traceback 的 error，宿主侧据此降级。
import sys
import os
import json
import importlib.util
import traceback

PLUGIN_DIR = sys.argv[1] if len(sys.argv) > 1 else "."
ENTRY = sys.argv[2] if len(sys.argv) > 2 else "main.py"
FUNC = sys.argv[3] if len(sys.argv) > 3 else "preview"
ACTION_FUNC = sys.argv[4] if len(sys.argv) > 4 else "action"

sys.path.insert(0, PLUGIN_DIR)

_import_err = None
_mod = None
try:
    _spec = importlib.util.spec_from_file_location("plugin_mod", os.path.join(PLUGIN_DIR, ENTRY))
    _mod = importlib.util.module_from_spec(_spec)
    _spec.loader.exec_module(_mod)
except Exception:
    _import_err = traceback.format_exc()


def _emit(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def _respond(rid, payload):
    if "error" in payload:
        _emit({"jsonrpc": "2.0", "id": rid, "error": payload["error"]})
    else:
        _emit({"jsonrpc": "2.0", "id": rid, "result": payload.get("result", {})})


def _call_preview(params):
    fn = getattr(_mod, FUNC, None)
    if fn is None:
        raise AttributeError("plugin has no %s()" % FUNC)
    return fn(params.get("path"), params.get("ctx") or {})


def _call_action(params):
    fn = getattr(_mod, ACTION_FUNC, None)
    if fn is None:
        raise AttributeError("plugin has no %s()" % ACTION_FUNC)
    return fn(params.get("name"), params.get("path"), params.get("ctx") or {})


def main():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except Exception as e:
            _emit({"jsonrpc": "2.0", "id": None, "error": {"message": "bad json: %s" % e}})
            continue
        rid = req.get("id")
        method = req.get("method")
        params = req.get("params") or {}
        if method not in ("preview", "action"):
            _respond(rid, {"error": {"message": "unknown method: %s" % method}})
            continue
        if _import_err is not None:
            _respond(rid, {"error": {"message": "plugin import failed:\n" + _import_err}})
            continue
        try:
            if method == "preview":
                res = _call_preview(params)
            else:
                res = _call_action(params)
            if res is None:
                res = {}
            # 动作类约定回 {"message":..}；容错把裸字符串也包成 message。
            if method == "action" and isinstance(res, str):
                res = {"message": res}
            _respond(rid, {"result": res})
        except Exception:
            _respond(rid, {"error": {"message": traceback.format_exc()}})


if __name__ == "__main__":
    main()
