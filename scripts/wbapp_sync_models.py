#!/usr/bin/env python3
"""wbapp_sync_models.py - 把本机网关作为「自定义模型」写进 WorkBuddy 桌面应用。

WorkBuddy 桌面端的自定义模型清单在 `~/.workbuddy/models.json`（一个数组，
每条 = 一个可在模型下拉里选的模型）。本脚本以网关 `/v1/models` 为真源，
生成指向 `http://127.0.0.1:7863/v1/chat/completions` 的条目。

用法：
    python scripts/wbapp_sync_models.py               # 写入精选模型（默认 8 个）
    python scripts/wbapp_sync_models.py --all         # 写入网关全部可用模型
    python scripts/wbapp_sync_models.py --dry-run     # 只打印将写入的条目
    python scripts/wbapp_sync_models.py --list        # 列出文件里现有条目

关键约束（踩过的坑，别改）：
  * `id` 会被原样当作请求体的 model 发出去，所以必须是网关认识的模型名；
  * 用户已有的 8788 条目 id 就叫 `deepseek-v4.1-flash`，直接同 id 会撞车，
    因此网关条目统一用网关的 `cn:` 前缀形式（网关同样认识，且天然不重名）；
  * `url` 要带完整 `/v1/chat/completions`（不是 base_url）；
  * 网关 api_key 为空，但应用侧要求非空字符串 → 填占位 `local-no-auth`。
"""

import argparse
import io
import json
import os
import shutil
import sys
import datetime
import urllib.request

MODELS_JSON = os.path.expanduser(r"~\.workbuddy\models.json")
GATEWAY = "http://127.0.0.1:7863"
ENDPOINT = GATEWAY + "/v1/chat/completions"
PLACEHOLDER_KEY = "local-no-auth"
CONFIG = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "config.json")


def effort_suffix():
    """应用的自定义模型 schema 只有 supportsReasoning 布尔，没有 effort 档位字段。
    所以把网关的 min_effort 写进显示名，让下拉里一眼看出"这条其实按 max 思考"。"""
    try:
        with io.open(CONFIG, encoding="utf-8") as f:
            cfg = json.load(f)
        me = ((cfg.get("upstream") or {}).get("min_effort") or "").strip()
        if me:
            return " · 推理" + me
    except Exception:
        pass
    return ""
FREE_TIER = {"hy3", "hy4free", "hy4flash-free", "hy4-preview-f"}  # 用户明确不用免费档

# 默认精选：覆盖"主力 / 便宜 / 免费 / 长文 / 视觉"几类，下拉不至于太长。
DEFAULT_PICKS = [
    "deepseek-v4.1-flash",   # 主力，x0.11，1M 上下文 + 原生多模态
    "deepseek-v4-pro",       # 更强，x0.51
    "glm-5.3-flash",         # 便宜快，x0.08
    "glm-5.3",               # 旗舰，x0.69
    "space-bunny",           # x0.08
    "minimax-m3.1-flash",    # x0.08
    "kimi-k3-1",             # 超长输出，x1.62
]


def safe(t):
    t = str(t)
    return t if t.isascii() else t.encode("unicode_escape").decode("ascii")


def fetch_models():
    with urllib.request.urlopen(GATEWAY + "/v1/models", timeout=10) as r:
        payload = json.load(r)
    out = {}
    for m in payload.get("data") or []:
        mid = (m.get("id") or "").strip()
        if not mid:
            continue
        bare = mid.split(":", 1)[1] if ":" in mid and mid.split(":", 1)[0] in ("cn", "global") else mid
        if bare in ("auto",) or bare in FREE_TIER or bare.startswith("hy4-preview"):
            continue
        entry = {
            "id": "cn:" + bare,
            "name": "%s (WorkBuddy 本机) %s%s" % (bare, (m.get("credits") or "").strip(), EFFORT_SUFFIX),
            "vendor": "Custom",
            "url": ENDPOINT,
            "apiKey": PLACEHOLDER_KEY,
            "supportsToolCall": bool(m.get("supports_tool_call", True)),
            "supportsImages": bool(m.get("supports_images", False)),
            "supportsReasoning": bool(m.get("supports_reasoning", False)),
            "useCustomProtocol": False,
            "maxInputTokens": int(m.get("context_length") or 0) or 131072,
            "maxOutputTokens": int(m.get("max_output_tokens") or 0) or 65536,
        }
        # 输出预算上限：照抄模型硬上限（deepseek 可达 393216）会让每个请求都申请巨额输出。
        if entry["maxOutputTokens"] > 65536:
            entry["maxOutputTokens"] = 65536
        if entry["maxOutputTokens"] > entry["maxInputTokens"]:
            entry["maxOutputTokens"] = entry["maxInputTokens"]
        out[bare] = entry
    return out


def load():
    if not os.path.exists(MODELS_JSON):
        return []
    with io.open(MODELS_JSON, encoding="utf-8") as f:
        data = json.load(f)
    if not isinstance(data, list):
        raise SystemExit("FAIL: %s 顶层不是数组" % safe(MODELS_JSON))
    return data


def cmd_list():
    items = load()
    print("path : %s" % safe(MODELS_JSON))
    print("count: %d" % len(items))
    for it in items:
        print("  - %-28s -> %-46s %s" % (it.get("id"), it.get("url"), safe(it.get("name"))))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--all", action="store_true")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--list", action="store_true")
    args = ap.parse_args()

    if args.list:
        cmd_list()
        return

    global EFFORT_SUFFIX
    EFFORT_SUFFIX = effort_suffix()
    gw = fetch_models()
    names = sorted(gw.keys()) if args.all else [n for n in DEFAULT_PICKS if n in gw]
    missing = [n for n in DEFAULT_PICKS if n not in gw] if not args.all else []
    if missing:
        print("note : 这些默认模型当前网关没有，已跳过: %s" % ", ".join(missing))
    new_entries = [gw[n] for n in names]
    print("gateway: %d entries to write" % len(new_entries))

    if args.dry_run:
        print(json.dumps(new_entries, ensure_ascii=False, indent=2))
        return

    items = load()
    # 幂等：先摘掉所有指向本网关的旧条目，再把新的接上去。
    kept = [it for it in items if not str(it.get("url", "")).startswith(GATEWAY)]
    removed = len(items) - len(kept)
    merged = kept + new_entries

    ts = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
    bak = MODELS_JSON + ".bak-wb2api-" + ts
    shutil.copy2(MODELS_JSON, bak)
    with io.open(MODELS_JSON, "w", encoding="utf-8") as f:
        json.dump(merged, f, ensure_ascii=False, indent=2)
        f.write("\n")

    # 写后校验：JSON 可解析、条目数对得上、网关条目 url 正确。
    check = load()
    if len(check) != len(merged):
        shutil.copy2(bak, MODELS_JSON)
        raise SystemExit("FAIL: 条目数不符，已回滚")
    gw_now = [it for it in check if str(it.get("url", "")).startswith(GATEWAY)]
    if len(gw_now) != len(new_entries):
        shutil.copy2(bak, MODELS_JSON)
        raise SystemExit("FAIL: 网关条目数不符，已回滚")

    print("removed: %d old gateway entry(ies)" % removed)
    print("kept   : %d foreign entry(ies)" % len(kept))
    print("written: %d gateway entries -> %s" % (len(gw_now), safe(MODELS_JSON)))
    print("backup : %s" % safe(os.path.basename(bak)))
    print("note   : 需要完全退出并重启 WorkBuddy 应用，模型下拉里才会出现")


if __name__ == "__main__":
    main()
