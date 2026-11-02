#!/usr/bin/env python3
"""
Agent 级可用性测试：用真实 Codex CLI 跑一批有客观判据的任务，
检验「Codex -> codex-router -> sais2api -> 星河启智」这条链路能不能当
正经编码 Agent 用。

和 docs/BENCHMARK.md 的区别：那边测的是模型裸聊水平（一轮问答、看输出质量），
这边测的是**工具回路**——模型会不会真的发 tool_call、参数对不对、拿到结果后
会不会继续、失败后会不会自己改，以及最终产物能不能通过机器校验。

用法：
    python3 bench/agent_eval.py                 # 跑全部任务
    python3 bench/agent_eval.py -t t03 t09      # 只跑指定任务
    python3 bench/agent_eval.py --runs 3        # 每个任务重复 3 次（看稳定性）
    python3 bench/agent_eval.py --out result.md # 另存报告

判据全部是「看文件 / 跑测试」，不看模型怎么夸自己。
"""

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import time

CODEX = os.environ.get(
    "CODEX_BIN",
    "/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex",
)
MODEL = os.environ.get("BENCH_MODEL", "星河启智-deepseek-v4.1-flash")
ROOT = os.environ.get("BENCH_ROOT", "/tmp/abench")
TIMEOUT = int(os.environ.get("BENCH_TIMEOUT", "420"))


# ---------------------------------------------------------------- 任务定义
# setup(dir) 铺初始现场，prompt 给 Codex，check(dir) 返回 (是否通过, 说明)。
# check 只看产物，不看模型的话术。

def _write(path, text):
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        f.write(text)


def _read(path):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return ""


def check_pelican_svg(d):
    p = os.path.join(d, "pelican.svg")
    if not os.path.exists(p):
        return False, "没有 pelican.svg"
    s = _read(p)
    if "<svg" not in s:
        return False, "不是 SVG"
    if len(s) < 600:
        return False, "SVG 过短 %d 字节，形同占位" % len(s)
    circles = len(re.findall(r"<circle", s))
    if circles < 2:
        return False, "只有 %d 个 <circle>，画不出两个轮子" % circles
    try:
        import xml.etree.ElementTree as ET
        ET.fromstring(s)
    except Exception as e:
        return False, "XML 不合法: %s" % e
    return True, "%d 字节 / %d 个圆 / XML 合法" % (len(s), circles)


def check_pelican_html(d):
    p = os.path.join(d, "pelican.html")
    if not os.path.exists(p):
        return False, "没有 pelican.html"
    s = _read(p).lower()
    need = {
        "doctype": "<!doctype html" in s,
        "svg": "<svg" in s,
        "轮子": s.count("<circle") >= 2 or s.count("<ellipse") >= 2,
        "动画": ("@keyframes" in s) or ("animatetransform" in s) or ("animation:" in s),
        "自包含": ("http://" not in s.replace("http://www.w3.org", "")) and (".png" not in s) and (".jpg" not in s),
    }
    bad = [k for k, v in need.items() if not v]
    if bad:
        return False, "缺: " + "/".join(bad)
    return True, "%d 字节，动画与轮子齐" % len(s)


FIXTURE_BUGGY = '''\
"""价格计算：满减 + 折扣。测试在 test_price.py。"""


def final_price(items, coupon=None):
    total = 0
    for it in items:
        total += it["price"] * it["qty"]
    if coupon == "SAVE10":
        total = total * 0.9
    if total > 200:
        total = total - 20
    return round(total, 2)
'''

FIXTURE_BUGGY_TEST = '''\
import unittest

from price import final_price


class PriceTest(unittest.TestCase):
    def test_no_coupon(self):
        self.assertEqual(final_price([{"price": 50, "qty": 2}]), 120.0)

    def test_coupon_then_threshold(self):
        # 9 折后仍超过 200，应继续减 20：300*0.9=270 -> 250
        self.assertEqual(final_price([{"price": 150, "qty": 2}], "SAVE10"), 250.0)

    def test_threshold_only(self):
        self.assertEqual(final_price([{"price": 100, "qty": 3}]), 280.0)


if __name__ == "__main__":
    unittest.main()
'''


def setup_bugfix(d):
    _write(os.path.join(d, "price.py"), FIXTURE_BUGGY)
    _write(os.path.join(d, "test_price.py"), FIXTURE_BUGGY_TEST)


def check_bugfix(d):
    try:
        r = subprocess.run([sys.executable, "-m", "unittest", "-q", "test_price"],
                           cwd=d, capture_output=True, text=True, timeout=120)
    except Exception as e:
        return False, "unittest 跑不起来: %s" % e
    out = (r.stdout + r.stderr).strip().splitlines()
    tail = out[-1] if out else ""
    if r.returncode == 0:
        return True, tail[:80]
    return False, "unittest 失败: " + tail[:100]


def setup_shell(d):
    for i in range(1, 13):
        ext = ["txt", "log", "md", "csv"][i % 4]
        _write(os.path.join(d, "data", "f%02d.%s" % (i, ext)), "x" * (i * 100))


def check_shell(d):
    p = os.path.join(d, "report.md")
    if not os.path.exists(p):
        return False, "没有 report.md"
    s = _read(p)
    if not re.search(r"(?<!\d)3(?!\d)", s):
        return False, "没写 .txt 文件数量（应为 3），内容: %r" % s[:120]
    return True, "%d 字节" % len(s)


def check_parallel(d):
    p = os.path.join(d, "both.txt")
    if not os.path.exists(p):
        return False, "没有 both.txt"
    s = _read(p).strip()
    if not re.search(r"Darwin|Linux", s):
        return False, "没有 uname 结果: %r" % s[:80]
    if not re.search(r"\d", s):
        return False, "没有时间输出: %r" % s[:80]
    return True, s.replace("\n", " ")[:60]


def check_csv(d):
    p = os.path.join(d, "sales.csv")
    if not os.path.exists(p):
        return False, "没有 sales.csv"
    lines = [l for l in _read(p).splitlines() if l.strip()]
    if not lines or "date" not in lines[0].lower():
        return False, "表头不对: %r" % (lines[0] if lines else "")
    if len(lines) < 201:
        return False, "只有 %d 行（表头 + 200 条应为 201）" % len(lines)
    return True, "%d 行" % len(lines)


def setup_recovery(d):
    _write(os.path.join(d, "app.conf"), "# 服务配置\nhost = 127.0.0.1\nport = 8080\nmode = prod\n")


def check_recovery(d):
    p = os.path.join(d, "port.txt")
    if not os.path.exists(p):
        return False, "没有 port.txt"
    s = _read(p).strip()
    if "8080" not in s:
        return False, "port 值不对: %r" % s[:80]
    return True, s[:40]


def setup_no_config(d):
    _write(os.path.join(d, "README.txt"), "这是一个空项目，只有说明文件。\n")


def check_no_config(d):
    p = os.path.join(d, "no-config.txt")
    if not os.path.exists(p):
        return False, "没有 no-config.txt（它可能编了一个 db.host）"
    s = _read(p).lower()
    if re.search(r"(db\.host|127\.0\.0\.1|localhost|:\d{4})", s):
        return False, "疑似编造了配置值: %r" % s[:100]
    return True, s.replace("\n", " ")[:60]


def setup_grep(d):
    lines = []
    for i in range(1, 4001):
        if i == 3000:
            lines.append("2026-11-02T03:00:00Z ERROR order=ORD-3000 msg=\"payment gateway timeout after 30s\"")
        elif i % 500 == 0:
            lines.append("2026-11-02T02:00:00Z WARN order=ORD-%d msg=\"retry scheduled\"" % i)
        else:
            lines.append("2026-11-02T01:00:00Z INFO order=ORD-%d msg=\"ok\"" % i)
    _write(os.path.join(d, "big.log"), "\n".join(lines) + "\n")


def check_grep(d):
    p = os.path.join(d, "line3000.txt")
    if not os.path.exists(p):
        return False, "没有 line3000.txt"
    s = _read(p)
    if "ORD-3000" not in s or "payment gateway timeout" not in s:
        return False, "内容不对: %r" % s[:120]
    return True, "命中 ORD-3000"


def check_fib(d):
    p = os.path.join(d, "fib.out")
    if not os.path.exists(p):
        return False, "没有 fib.out"
    nums = re.findall(r"\d+", _read(p))
    if len(nums) < 20:
        return False, "只找到 %d 个数字" % len(nums)
    expect = [0, 1, 1, 2, 3, 5, 8, 13, 21, 34, 55, 89, 144, 233, 377, 610, 987, 1597, 2584, 4181]
    got = [int(x) for x in nums[:20]]
    if got != expect:
        return False, "前 20 项不对: %s" % got[:8]
    if not os.path.exists(os.path.join(d, "fib.py")):
        return False, "没有 fib.py"
    return True, "20 项正确"


def check_tests(d):
    p = os.path.join(d, "calc.py")
    if not os.path.exists(p):
        return False, "没有 calc.py"
    try:
        r = subprocess.run([sys.executable, "-m", "unittest", "-q", "test_calc"],
                           cwd=d, capture_output=True, text=True, timeout=120)
        if r.returncode == 0:
            return True, "自写测试通过"
    except Exception:
        pass
    # 没写测试但函数正确也算过
    try:
        sys.path.insert(0, d)
        import importlib
        m = importlib.import_module("calc")
        if m.safe_div(6, 3) == 2 and m.safe_div(1, 0) is None:
            return True, "函数正确（未写测试）"
    except Exception as e:
        return False, "函数不可用: %s" % str(e)[:80]
    return False, "safe_div 行为不对"


TASKS = [
    {
        "id": "t01",
        "name": "鹈鹕骑自行车（SVG）",
        "why": "经典空间推理题，考的是「先写文件再自检」的回路",
        "prompt": "Generate an SVG of a pelican riding a bicycle. "
                  "Save it as pelican.svg in the current directory. "
                  "Do not use external image assets. After writing, verify the file exists and is valid XML.",
        "setup": None,
        "check": check_pelican_svg,
        "timeout": 480,
    },
    {
        "id": "t02",
        "name": "鹈鹕骑车（HTML 动画）",
        "why": "动画版，要求轮子真的转起来，比静态图更容易暴露编造",
        "prompt": "写一个自包含的 pelican.html：用内联 SVG 画一只骑自行车的鹈鹕，"
                  "两个轮子要转、踏板要跟着动（CSS 动画或 SMIL 都行）。"
                  "不许引用任何外部图片或 JS。写完用命令确认文件存在、大小合理。",
        "setup": None,
        "check": check_pelican_html,
        "timeout": 480,
    },
    {
        "id": "t03",
        "name": "修 bug 到测试通过",
        "why": "读代码、改代码、跑测试的完整闭环",
        "prompt": "price.py 里 final_price 有 bug，test_price.py 是验收测试（标准库 unittest）。"
                  "先跑 `python3 -m unittest -q test_price` 看失败，定位原因，改 price.py 让测试通过，最后再跑一次确认。不要改测试文件。",
        "setup": setup_bugfix,
        "check": check_bugfix,
        "timeout": 480,
    },
    {
        "id": "t04",
        "name": "多步 shell 统计",
        "why": "连续多条命令 + 中间结果汇总，考计划性",
        "prompt": "工作目录的 data/ 下有一堆文件。统计其中 .txt 文件的个数，"
                  "并按文件大小列出最大的 3 个。把结论写进 report.md（要写明 .txt 数量）。",
        "setup": setup_shell,
        "check": check_shell,
        "timeout": 450,
    },
    {
        "id": "t05",
        "name": "并发多条命令",
        "why": "测一轮里发多个 tool_call 的能力（并行调用）",
        "prompt": "请在同一轮里同时执行两条独立命令：`uname -s` 和 `date +%H:%M:%S`（或任意能打印时间的命令）。"
                  "把两条结果都写进 both.txt，一行一条。",
        "setup": None,
        "check": check_parallel,
        "timeout": 480,
    },
    {
        "id": "t06",
        "name": "长输出落盘",
        "why": "输出被截断是长任务里最常见的坑",
        "prompt": "生成 sales.csv：表头 date,amount，然后 200 行数据，date 从 2026-01-01 起每天一行，"
                  "amount 是 1-100 的整数。写完用 wc -l 确认是 201 行。",
        "setup": None,
        "check": check_csv,
        "timeout": 450,
    },
    {
        "id": "t07",
        "name": "失败后换路子",
        "why": "工具报错后能不能改参数继续，而不是停工",
        "prompt": "先尝试读 /nonexistent/foo.conf 里的 port。读不到就退而看工作目录下的 app.conf，"
                  "把 port 的值写进 port.txt（只写数字）。",
        "setup": setup_recovery,
        "check": check_recovery,
        "timeout": 480,
    },
    {
        "id": "t08",
        "name": "不许编造",
        "why": "上一轮那条假 Tool not found 的同类问题：没有的东西不能顺着说",
        "prompt": "工作目录里有没有 config.yaml？有就把它 db.host 的值写进 host.txt；"
                  "没有就写 no-config.txt，内容说明该项目没有 config.yaml。不要创建 config.yaml。",
        "setup": setup_no_config,
        "check": check_no_config,
        "timeout": 480,
    },
    {
        "id": "t09",
        "name": "4000 行日志里定位",
        "why": "不能把整份文件塞进上下文，必须用 grep/sed 定向取",
        "prompt": "big.log 有 4000 行。把第 3000 行的整行内容原样写进 line3000.txt（不要加引号或额外说明），"
                  "然后 cat 出来确认。",
        "setup": setup_grep,
        "check": check_grep,
        "timeout": 450,
    },
    {
        "id": "t10",
        "name": "中文需求 + 代码 + 执行",
        "why": "中文指令理解、写脚本、跑脚本、落盘输出一条龙",
        "prompt": "用 Python 写 fib.py，打印斐波那契数列前 20 项（从 0 开始，空格或换行分隔）。"
                  "运行它，把输出存进 fib.out。",
        "setup": None,
        "check": check_fib,
        "timeout": 480,
    },
    {
        "id": "t11",
        "name": "写实现 + 写测试",
        "why": "测它会不会自己造验收（agent 的自我校验意识）",
        "prompt": "写 calc.py，实现 safe_div(a, b)：正常相除返回浮点结果，除数为 0 时返回 None。"
                  "同时在 test_calc.py 写 unittest 用例覆盖这两种情况，跑一遍确认通过。",
        "setup": None,
        "check": check_tests,
        "timeout": 450,
    },
]


# ---------------------------------------------------------------- 执行
def run_task(task, workdir):
    os.makedirs(workdir, exist_ok=True)
    if task["setup"]:
        task["setup"](workdir)

    cmd = [
        CODEX, "exec",
        "--skip-git-repo-check",
        "-C", workdir,
        "-s", "workspace-write",
        "-m", MODEL,
        "--json",
        task["prompt"],
    ]
    env = dict(os.environ)
    env.pop("CODEX_HOME", None)  # 用真实用户配置，和 GUI 一致

    started = time.time()
    try:
        proc = subprocess.run(cmd, cwd=workdir, capture_output=True, text=True,
                              timeout=task.get("timeout", TIMEOUT), stdin=subprocess.DEVNULL, env=env)
        raw, err, code = proc.stdout, proc.stderr, proc.returncode
    except subprocess.TimeoutExpired as e:
        raw = (e.stdout or b"").decode("utf-8", "replace") if isinstance(e.stdout, bytes) else (e.stdout or "")
        err = "TIMEOUT"
        code = -1
    wall = time.time() - started

    events = []
    for line in raw.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            events.append(json.loads(line))
        except Exception:
            pass

    cmds, tokens, turns, msgs = [], 0, 0, []
    errors = []
    for ev in events:
        t = ev.get("type")
        if t == "turn.completed":
            turns += 1
            u = ev.get("usage") or {}
            tokens += int(u.get("input_tokens") or 0) + int(u.get("output_tokens") or 0)
        if t == "item.completed":
            it = ev.get("item") or {}
            if it.get("type") == "command_execution":
                cmds.append((it.get("command") or "")[:160])
                if it.get("exit_code") not in (0, None):
                    errors.append("exit %s: %s" % (it.get("exit_code"), (it.get("aggregated_output") or "")[:120]))
            elif it.get("type") == "agent_message":
                msgs.append(it.get("text") or "")
            elif it.get("type") == "error":
                errors.append(str(it.get("message"))[:160])

    ok, detail = False, "未执行"
    try:
        ok, detail = task["check"](workdir)
    except Exception as e:
        detail = "校验异常: %s" % e

    return {
        "id": task["id"],
        "name": task["name"],
        "ok": ok,
        "detail": detail,
        "wall": round(wall, 1),
        "tool_calls": len(cmds),
        "turns": turns or 1,
        "tokens": tokens,
        "exit": code,
        "errors": errors[:3],
        "last_msg": (msgs[-1] if msgs else "")[:400],
        "cmds": cmds[:12],
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-t", "--tasks", nargs="*", default=None)
    ap.add_argument("--runs", type=int, default=1)
    ap.add_argument("--out", default=None)
    ap.add_argument("--keep", action="store_true", help="保留每轮工作目录")
    args = ap.parse_args()

    if not os.path.exists(CODEX):
        sys.exit("找不到 Codex CLI: %s（用 CODEX_BIN 指定）" % CODEX)

    picked = [t for t in TASKS if not args.tasks or t["id"] in args.tasks]
    if not picked:
        sys.exit("没有匹配的任务")

    results = []
    for t in picked:
        for run in range(1, args.runs + 1):
            wd = os.path.join(ROOT, t["id"], "run%d" % run)
            shutil.rmtree(wd, ignore_errors=True)
            print("▶ %s %s (run %d/%d)" % (t["id"], t["name"], run, args.runs), flush=True)
            r = run_task(t, wd)
            r["run"] = run
            results.append(r)
            mark = "PASS" if r["ok"] else "FAIL"
            print("  %s %5.0fs  tools=%d  tokens=%d  %s"
                  % (mark, r["wall"], r["tool_calls"], r["tokens"], r["detail"][:70]), flush=True)
            if r["errors"]:
                print("  ! " + " | ".join(r["errors"])[:200], flush=True)
            if not args.keep:
                shutil.rmtree(wd, ignore_errors=True)

    total = len(results)
    passed = sum(1 for r in results if r["ok"])
    print("\n== 汇总：%d/%d 通过 ==" % (passed, total))
    print("| 任务 | 结果 | 耗时 | 工具调用 | tokens | 说明 |")
    print("| --- | --- | --- | --- | --- | --- |")
    for r in results:
        print("| %s %s | %s | %.0fs | %d | %d | %s |"
              % (r["id"], r["name"], "✅" if r["ok"] else "❌",
                 r["wall"], r["tool_calls"], r["tokens"], r["detail"][:60].replace("|", "/")))

    blob = {"model": MODEL, "codex": CODEX, "time": time.strftime("%Y-%m-%d %H:%M:%S"),
            "passed": passed, "total": total, "results": results}
    default_out = os.path.join(ROOT, "result.json")
    with open(default_out, "w", encoding="utf-8") as f:
        json.dump(blob, f, ensure_ascii=False, indent=2)
    print("\n明细: %s" % default_out)
    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            json.dump(blob, f, ensure_ascii=False, indent=2)
    sys.exit(0 if passed == total else 1)


if __name__ == "__main__":
    main()
