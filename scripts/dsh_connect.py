# flow: 用 python 精确插入/移除 provider（YAML 结构化，不靠行号猜）
import sys, io, re, json, shutil, datetime, os
import yaml

YML = os.path.expanduser(r"~\.dsh\profiles\desktop\cordis.patch.yml")
CRED = os.path.expanduser(r"~\.dsh\.credentials.yaml")
KEY = "WORKBUDDY_LOCAL_KEY"
PROVIDER = "workbuddy-local"

def load_yaml_text(path):
    with io.open(path, encoding="utf-8") as f:
        return f.read()

def backup(path):
    ts = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
    dst = path + ".bak-wb-" + ts
    shutil.copy2(path, dst)
    return dst

def validate(path):
    with io.open(path, encoding="utf-8") as f:
        data = yaml.safe_load(f)
    if not isinstance(data, list):
        raise SystemExit("YAML 顶层不是数组")
    for item in data:
        if isinstance(item, dict) and item.get("id") == "llm-pi-ai":
            return item
    raise SystemExit("没找到 llm-pi-ai 条目")

def find_providers_span(lines):
    """返回 (start, end)：providers 映射的行区间（0-based，end 不含）。"""
    start = None
    for i, ln in enumerate(lines):
        if re.match(r"^\s+providers:\s*$", ln):
            start = i + 1
            break
    if start is None:
        raise SystemExit("没找到 providers: 行")
    # providers 的子项缩进
    child_indent = None
    end = len(lines)
    for j in range(start, len(lines)):
        ln = lines[j]
        if ln.strip() == "":
            continue
        if child_indent is None:
            child_indent = len(ln) - len(ln.lstrip(" "))
        cur = len(ln) - len(ln.lstrip(" "))
        if cur < child_indent:
            end = j
            break
    return start, end, child_indent

def safe(name):
    """控制台编码不一定是 UTF-8；非 ASCII 名字退化成转义形式，避免乱码。"""
    return name if str(name).isascii() else str(name).encode("unicode_escape").decode("ascii")

def cmd_status():
    item = validate(YML)
    provs = list((item["config"].get("providers") or {}).keys())
    print("config :", YML)
    print("providers:", ", ".join(safe(p) for p in provs))
    print("configured:", PROVIDER in provs)

def cmd_apply():
    # ① 凭据
    cred = load_yaml_text(CRED)
    if re.search(r"(?m)^\s*" + KEY + r":", cred):
        print("[credentials] already present, skipped")
    else:
        cred2 = re.sub(r"(?m)^(refs:\s*)$", r"\1\n  " + KEY + ": local-bridge-no-auth", cred)
        if cred2 == cred:
            raise SystemExit("credentials: 没找到 refs: 行")
        b = backup(CRED)
        with io.open(CRED, "w", encoding="utf-8") as f:
            f.write(cred2)
        print("[credentials] wrote " + KEY + " (backup " + os.path.basename(b) + ")")

    # ② provider
    validate(YML)  # 先证明当前文件是好的
    text = load_yaml_text(YML)
    lines = text.splitlines()
    if re.search(r"(?m)^\s+" + PROVIDER + r":", text):
        print("[cordis] already present, skipped")
        return
    start, end, indent = find_providers_span(lines)
    base = " " * indent
    block = [
        base + PROVIDER + ":",
        base + "  displayName: WorkBuddy（本机 7863）",
        base + "  api: openai-completions",
        base + "  baseURL: http://127.0.0.1:7863/v1",
        base + "  apiKeyEnv: " + KEY,
        base + "  compat:",
        base + "    thinkingFormat: deepseek",
        base + "  models:",
        base + "    - id: deepseek-v4.1-flash",
        base + "      name: deepseek-v4.1-flash (WorkBuddy)",
        base + "      contextWindow: 1000000",
        base + "      maxTokens: 65536",
        base + "    - id: glm-5.3-flash",
        base + "      name: glm-5.3-flash (WorkBuddy)",
        base + "      contextWindow: 131072",
        base + "      maxTokens: 65536",
        base + "    - id: hy3",
        base + "      name: hy3 (WorkBuddy)",
        base + "      contextWindow: 131072",
        base + "      maxTokens: 65536",
    ]
    new_lines = lines[:end] + block + ([""] if end < len(lines) and lines[end].strip() else []) + lines[end:]
    b = backup(YML)
    with io.open(YML, "w", encoding="utf-8") as f:
        f.write("\n".join(new_lines) + "\n")
    try:
        item = validate(YML)
        assert PROVIDER in (item["config"].get("providers") or {}), "新条目没进 providers"
    except Exception as e:
        shutil.copy2(b, YML)
        raise SystemExit("校验失败已自动回滚：%s" % e)
    print("[cordis] wrote " + PROVIDER + " (backup " + os.path.basename(b) + ")")

def cmd_remove():
    text = load_yaml_text(YML)
    lines = text.splitlines()
    if not re.search(r"(?m)^\s+" + PROVIDER + r":", text):
        print("[cordis] not present, skipped")
    else:
        start, end, indent = find_providers_span(lines)
        keep, i, removed = [], start, 0
        out = lines[:start]
        while i < end:
            ln = lines[i]
            cur = len(ln) - len(ln.lstrip(" ")) if ln.strip() else indent + 1
            if ln.strip() and cur == indent and re.match(r"\s*" + PROVIDER + r":\s*$", ln):
                i += 1
                while i < end:
                    nxt = lines[i]
                    if nxt.strip() == "":
                        i += 1
                        continue
                    c2 = len(nxt) - len(nxt.lstrip(" "))
                    if c2 <= indent:
                        break
                    i += 1
                removed += 1
                continue
            out.append(ln)
            i += 1
        out += lines[end:]
        b = backup(YML)
        with io.open(YML, "w", encoding="utf-8") as f:
            f.write("\n".join(out) + "\n")
        try:
            validate(YML)
        except Exception as e:
            shutil.copy2(b, YML)
            raise SystemExit("移除后校验失败，已回滚：%s" % e)
        print("[cordis] removed " + PROVIDER + " (blocks: %d)" % removed)

cmd = sys.argv[1]
{"status": cmd_status, "apply": cmd_apply, "remove": cmd_remove}[cmd]()