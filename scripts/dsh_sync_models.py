#!/usr/bin/env python3
"""dsh_sync_models.py - 把本机网关 /v1/models 的模型清单同步进 DSH 的 workbuddy-local provider。

用法（在 workbuddy2api-panel 目录下）：

    python scripts/dsh_sync_models.py            # 同步（写前备份、写后校验、失败回滚）
    python scripts/dsh_sync_models.py --dry-run   # 只打印将要写入的 models 块
    python scripts/dsh_sync_models.py --status    # 只看当前 provider 里有哪些模型

为什么需要：网关的模型目录是上游动态拉取的（19 个模型，还会变），DSH 的 provider
必须在配置文件里**静态列出**可选模型。手抄容易漏、容易写错 contextWindow。本脚本
以网关的 /v1/models 为唯一真源，重写该 provider 的 models: 块，其它内容一个字不动。

安全边界：
  * 只改 workbuddy-local 这个 provider 的 models 子块；
  * 写前备份 .bak-models-<时间戳>，写后 pyyaml 解析 + 结构断言，失败自动回滚；
  * 所有输出 ASCII-safe（控制台编码不一定是 UTF-8）。
"""

import argparse
import io
import json
import os
import re
import shutil
import sys
import datetime
import urllib.request

import yaml

YML = os.path.expanduser(r"~\.dsh\profiles\desktop\cordis.patch.yml")
PROVIDER = "workbuddy-local"
BASE = "http://127.0.0.1:7863"

# 网关里不建议塞进日常模型下拉的（省得选错）：auto 是路由占位；hy4-preview 无元数据。
SKIP_IDS = {"auto"}
# 免费档：用户明确不用（0 积分 / 免费预览）。同步时一律不写进 provider。
FREE_TIER = {"hy3", "hy4free", "hy4flash-free", "hy4-preview-f"}
OUTPUT_CAP = 65536  # 单次回复输出预算上限（见 fetch_models 注释）


def safe(text):
    text = str(text)
    return text if text.isascii() else text.encode("unicode_escape").decode("ascii")


def fetch_models():
    with urllib.request.urlopen(BASE + "/v1/models", timeout=10) as r:
        payload = json.load(r)
    out = []
    for m in payload.get("data") or []:
        mid = (m.get("id") or "").strip()
        if not mid:
            continue
        # 网关接受 "cn:xxx" / "global:xxx" / 裸名；provider 里统一写裸名（默认走 cn）。
        bare = mid.split(":", 1)[1] if ":" in mid and mid.split(":", 1)[0] in ("cn", "global") else mid
        if bare in SKIP_IDS or bare in FREE_TIER or bare.startswith("hy4-preview"):
            continue
        ctx = int(m.get("context_length") or 0) or 131072
        mx = int(m.get("max_output_tokens") or 0) or 65536
        # maxTokens 是"单次回复的输出预算上限"。上游给的是模型硬上限（deepseek 可达
        # 393216），照抄会让每个请求都申请巨额输出预算——浪费且更容易撞限流。
        # 65536 对聊天/编码场景足够宽松，也不会压掉正常回答（网关还有空回复救援兜底）。
        if mx > OUTPUT_CAP:
            mx = OUTPUT_CAP
        # 不能大于 contextWindow。
        if mx > ctx:
            mx = ctx
        out.append({
            "id": bare,
            "label": (m.get("name") or bare),
            "ratio": (m.get("credits") or ""),
            "contextWindow": ctx,
            "maxTokens": mx,
            # 图片（视觉）能力：必须显式声明，否则 DSH 按 DEFAULT_INPUT=['text'] 处理，
            # 会在提示词准入前就把图片拒绝/剥离，表现成"接上网关就识别不了图片"。
            "images": bool(m.get("supports_images")),
        })
    # 去重（cn:/global: 两个域可能有同名），保持网关返回顺序。
    seen, uniq = set(), []
    for x in out:
        if x["id"] in seen:
            continue
        seen.add(x["id"])
        uniq.append(x)
    return uniq


def model_block(models):
    lines = ["        models:"]
    for m in models:
        tag = m["id"]
        if m["ratio"]:
            tag = "%s %s" % (m["id"], m["ratio"])
        lines.append("          - id: %s" % m["id"])
        lines.append("            name: %s" % tag)
        lines.append("            contextWindow: %d" % m["contextWindow"])
        lines.append("            maxTokens: %d" % m["maxTokens"])
        # input 是"这个端点接受什么模态"的声明（pi-ai PiAiModelProfile.input）。
        # 不写 = 路由 defaultInput，再缺省就是 ['text'] → 图片会被上游前置拒绝。
        lines.append("            input: [%s]" % ("text, image" if m.get("images") else "text"))
    return lines


def find_span(lines):
    """返回 provider 块起始行号（含）与结束行号（不含）。"""
    start = None
    for i, ln in enumerate(lines):
        if re.match(r"^\s+" + PROVIDER + r":\s*$", ln):
            start = i
            break
    if start is None:
        raise SystemExit("FAIL: 没找到 provider %s" % PROVIDER)
    indent = len(lines[start]) - len(lines[start].lstrip(" "))
    end = len(lines)
    for j in range(start + 1, len(lines)):
        if not lines[j].strip():
            continue
        cur = len(lines[j]) - len(lines[j].lstrip(" "))
        if cur <= indent:
            end = j
            break
    return start, end


def models_span(lines, start, end):
    """在 provider 块内定位 models: 子块，返回 (start, end)。"""
    mstart = None
    for i in range(start, end):
        if re.match(r"^\s+models:\s*$", lines[i]):
            mstart = i
            break
    if mstart is None:
        return None, None
    mindent = len(lines[mstart]) - len(lines[mstart].lstrip(" "))
    mend = end
    for j in range(mstart + 1, end):
        if not lines[j].strip():
            continue
        cur = len(lines[j]) - len(lines[j].lstrip(" "))
        if cur <= mindent:
            mend = j
            break
    return mstart, mend


def validate(path):
    with io.open(path, encoding="utf-8") as f:
        data = yaml.safe_load(f)
    if not isinstance(data, list):
        raise SystemExit("FAIL: 顶层不是数组")
    for item in data:
        if isinstance(item, dict) and item.get("id") == "llm-pi-ai":
            provs = (item.get("config") or {}).get("providers") or {}
            if PROVIDER not in provs:
                raise SystemExit("FAIL: provider 丢了")
            return provs[PROVIDER].get("models") or []
    raise SystemExit("FAIL: 没找到 llm-pi-ai")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--status", action="store_true")
    args = ap.parse_args()

    if args.status:
        ms = validate(YML)
        print("provider: %s" % PROVIDER)
        print("models  : %d" % len(ms))
        for m in ms:
            print("  - %-24s ctx=%-8s max=%s" % (m.get("id"), m.get("contextWindow"), m.get("maxTokens")))
        return

    models = fetch_models()
    print("gateway : %d models (after skip)" % len(models))
    block = model_block(models)

    if args.dry_run:
        print("\n".join(block))
        return

    with io.open(YML, encoding="utf-8") as f:
        text = f.read()
    lines = text.splitlines()
    start, end = find_span(lines)
    mstart, mend = models_span(lines, start, end)
    if mstart is None:
        # provider 没有 models: 块 → 插到 provider 块末尾
        new_lines = lines[:end] + [""] + block + lines[end:]
    else:
        new_lines = lines[:mstart] + block + lines[mend:]

    ts = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
    bak = YML + ".bak-models-" + ts
    shutil.copy2(YML, bak)
    with io.open(YML, "w", encoding="utf-8") as f:
        f.write("\n".join(new_lines) + "\n")

    try:
        ms = validate(YML)
        if len(ms) != len(models):
            raise SystemExit("写入后模型数不符: %d vs %d" % (len(ms), len(models)))
    except SystemExit:
        shutil.copy2(bak, YML)
        raise
    except Exception as e:
        shutil.copy2(bak, YML)
        raise SystemExit("写入后校验失败已回滚: %s" % e)

    print("written : %d models -> %s" % (len(ms), safe(YML)))
    print("backup  : %s" % safe(os.path.basename(bak)))
    print("note    : 重启 DeepSeek Harness 后模型下拉才会更新")


if __name__ == "__main__":
    main()
