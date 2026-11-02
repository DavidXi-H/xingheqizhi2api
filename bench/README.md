# Agent 级可用性测试

`docs/BENCHMARK.md` 测的是**模型裸聊水平**：一轮提问，看回答、看 SVG 画得好不好。
这里测的是另一件事——**这条链路能不能当编码 Agent 用**：

```
Codex CLI ──responses──> codex-router:8787 ──chat──> sais2api:3067 ──> 星河启智
```

模型会不会真的发 tool_call、参数对不对、拿到结果会不会继续、命令失败会不会改路子、
最后产物能不能过机器校验。全部判据都是**看文件 / 跑测试**，不看模型怎么夸自己。

## 怎么跑

```bash
python3 bench/agent_eval.py                  # 全部任务
python3 bench/agent_eval.py -t t03 t09       # 只跑指定任务
python3 bench/agent_eval.py --runs 3         # 每个任务 3 次，看稳定性
python3 bench/agent_eval.py --keep           # 保留工作目录，失败时好翻现场

BENCH_MODEL=商汤-deepseek-v4.1-flash python3 bench/agent_eval.py   # 换模型对比
```

前置条件：`codex` CLI 在默认路径（`/Applications/ChatGPT.app/Contents/Resources/codex-cli/…`，
可用 `CODEX_BIN` 覆盖）、`~/.codex/config.toml` 指向本地路由、router 与 sais2api 都在跑。
工作目录在 `/tmp/abench/<任务号>/run<N>`，每次开跑前清空。

## 任务清单

| 编号 | 任务 | 考的是什么 | 判据 |
| --- | --- | --- | --- |
| t01 | 鹈鹕骑自行车（SVG） | 经典空间推理题 + 先写文件再自检 | `pelican.svg` 存在、XML 合法、≥2 个圆、≥600 字节 |
| t02 | 鹈鹕骑车（HTML 动画） | 动画版，轮子得真转，比静态图更容易暴露编造 | `pelican.html` 有 doctype/svg/轮子/动画关键字，且不引用外部资源 |
| t03 | 修 bug 到测试通过 | 读代码、改代码、跑测试的完整闭环 | `pytest` 全绿，且测试文件没被改 |
| t04 | 多步 shell 统计 | 连续多条命令 + 中间结果汇总 | `report.md` 写明 `.txt` 数量为 3 |
| t05 | 并发多条命令 | 一轮里发多个 tool_call | `both.txt` 同时含系统和时间输出 |
| t06 | 长输出落盘 | 输出被截断是长任务里最常见的坑 | `sales.csv` 表头 + 200 行 |
| t07 | 失败后换路子 | 命令报错后改参数继续，而不是停工 | 先读不存在的路径失败，最终 `port.txt` 为 8080 |
| t08 | 不许编造 | 没有的东西不能顺着说（对应"假 Tool not found"那类事故） | 不存在 `config.yaml` 时必须产出 `no-config.txt`，且不出现伪造的 host 值 |
| t09 | 4000 行日志里定位 | 不能整份塞进上下文，必须 grep/sed 定向取 | `line3000.txt` 命中 `ORD-3000` 那一行 |
| t10 | 中文需求 + 代码 + 执行 | 中文指令、写脚本、跑脚本、落盘一条龙 | `fib.out` 前 20 项完全正确 |
| t11 | 写实现 + 写测试 | 会不会自己造验收 | `pytest test_calc.py` 通过，或 `safe_div` 行为正确 |

## 结果怎么读

脚本最后打印一张表并写 `/tmp/abench/result.json`，每个任务记录：
耗时、执行的命令条数（tool_calls）、token 用量、失败原因、模型最后一段话。

`tool_calls = 0` 而任务又失败了，基本就是**模型没调工具就作答**，属于本链路最典型的故障，
具体表现见下一节。

## 已知故障与对策

星河启智上游「大圣」本身就是个带工作区的 Agent，这带来三个特有毛病，
`sais2api` 里各有一条对策（都在 `main.go` / `tools.go`）：

| 现象 | 原因 | 对策 |
| --- | --- | --- |
| 模型说"已创建文件"但目录里没有，回复里还带 `<file data-path=…>` 卡片 | 它在自己沙箱里把活干完了，用户拿不到 | 检测「零调用却声称完成」，追加纠正话术重问，最多两轮（`toolCallNudge` / `toolCallNudgeFile`） |
| 模型说 `Tool exec_command not found`、要求用户"恢复连接" | 同样是自己沙箱里对不上号后编出来的解释 | 协议里禁止编造工具报错；工具结果已给却宣称工具坏掉时也重问 |
| 客户端只收到半截 `<tool_call>{"name":…` | 上游流式还没吐完，前面内容块的 `ended_at` 已经置位 | `lastEnded` 只看最后一个 text 块；未闭合的 tool_call 要等文本稳定 4 个轮询周期才收工 |
| `Error: exceeded retry limit, last status: 429` | 上游限流透传给 Codex，它自己的重试额度很少 | 上游 408/425/429/5xx 在 sais2api 内退避重试（2s→4s→8s，最多 4 次）；路由器在整链都失败时再整链重试 |

还有一条不是 bug 的限制：**模型看不到图**（`view_image` 在本链路不可用，
`models.json` 里 `input_modalities` 只有 text）。需要判断截图时让它用
`sips` / `python3 + PIL` 读像素，比让它"看图"可靠。

## 排查手法

- 服务端事件：`curl --noproxy '*' -H "X-Admin-Key: <admin_key>" http://127.0.0.1:3067/admin/api/status`
- 原始 prompt 与回复：`launchctl setenv SAIS_DUMP_DIR /tmp/saisdump` 后重启服务，
  每次请求落一份 `HHMMSS.mmm_<id>.txt`，含 PROMPT / RAW / PARSED / 重问记录
- Codex 侧会话：`~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`，
  看 `response_item` 里的 `function_call` / `function_call_output` 才是真的发生过的调用，
  assistant 正文里说的"工具返回了什么"一律不可信
