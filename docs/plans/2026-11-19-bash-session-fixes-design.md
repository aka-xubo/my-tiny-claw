# Bash 会话修复设计（HTW-2102）

本修订对应工单已确认的六项修复范围，基于 `0d6cdeb`。涉及只读 bash 的行为以本文件为准，替代早期设计与实施计划中的字符串白名单方案。

## 输出与游标

已结束会话的首次返回，以及未提供显式 cursor 的 poll，只读取 serverCursor 之后的未读内容。未读内容超过 max_output_bytes 时返回该范围的尾部，优先保留最终错误，同时推进默认游标到日志末尾。未读日志被淘汰或响应被截断时设置 output_truncated；已读内容不重发，游标到达末尾后的连续 poll 返回空输出和最终状态。显式 cursor 始终保留从该位置重放的语义，且不改变默认游标。运行中的会话继续消费增量输出。

## 进程清理与并发

Stop 和 Shutdown 不以外层 bash 的运行状态判断进程组是否需要清理。所有目标组先收到 SIGTERM，统一等待最多两秒，再统一发送 SIGKILL，并等待 Go 的 Wait goroutine 完成回收。Unix 检查整个组是否仍存在，避免 leader 已退出而后代忽略 SIGTERM 时跳过强制终止。

Shutdown 关闭准入后等待已预占启动名额的 Start 完成，再快照所有会话，避免 fork/exec 与退出并发时漏掉进程。全局锁只保护预占名额和会话表；cmd.Start、信号发送、回收均在锁外。并发 Shutdown 共享一次清理结果。淘汰结束会话时，同样在锁外清理它的进程组，防止丢掉后代进程的清理句柄。

仅对本 Manager 创建并仍持有记录的组发送信号；空 PGID 可能被操作系统复用的理论风险仍存在，ESRCH 被视为正常情况，不回退到单 PID 信号。主动 setsid/逃离进程组的后代不在 Unix 进程组清理保证内。Windows 本轮仍只终止直接子进程，Job Object 继续延期。

signal 字段来自真实 ProcessState：普通非零退出不填信号，SIGKILL 不再被误报为预设的 SIGTERM。已 yield 的会话不再监听单次调用 Context；启动与前台 WaitUntil 仍处理取消，后台会话由显式 Stop/Shutdown 管理。

## 子 Agent 的只读 bash

选择系统隔离：相比命令 AST 加逐命令参数白名单，它能覆盖外部脚本、Git 配置、预处理器和未来新增参数的间接写入。删除只读约束会失去已确认的只读要求；继续扩充黑名单无法构成可靠边界。

- macOS：固定使用 `/usr/bin/sandbox-exec`，默认拒绝，只允许读取、exec/fork 和 sysctl 读取；允许写 `/dev/null` 供 Git 等工具使用。网络、Mach IPC 和普通文件写入没有授权。
- Linux amd64/arm64：固定使用 `/usr/bin/bwrap`，需要管理员预装 bubblewrap、内核允许用户命名空间及 seccomp。根文件系统只读，独立 user/PID/network/IPC 等命名空间，不提供工作区或临时目录的可写挂载。seccomp 额外拒绝 socket/connect（包括只读挂载仍可访问的宿主 Unix socket）、ptrace/setns/unshare 和可绕过 socket 过滤的 io_uring。不同 syscall ABI 被拒绝。
- 其他系统/架构，或隔离程序缺失、隔离初始化失败：拒绝执行，不退回普通 bash。

Middleware 仅校验一次性调用协议，工具自身负责隔离，因此直接调用工具也不能绕过。引号、管道、嵌套 shell、外部脚本按正常 shell 语法执行，后代继承同一限制；禁止的是写入效果，不再根据命令名称猜测。安装器的网络访问与文件写入均受限。

只读模式保留同步执行，最多 30 秒超时，输出内存上限 64 KiB、响应保留最后 8000 字节，退出或超时时清理进程组。命令非零退出（包括 grep 无匹配）按普通同步 bash 约定返回报错文本，Registry 的 IsError 为 false；超时提供明确的超时与终止提示。成功、失败、超时路径均保留截断提示。隔离命令无法构造或启动时仍返回工具错误，不回退执行。工具描述说明不能在 /tmp 等位置创建临时文件。隔离不限制读取到工作区，也不提供 CPU/进程数配额。

## 保留的行为

首次与 poll 的默认 yield_time_ms 继续为 10000，最大 30000。Feishu 按 chat 隔离、RecoveryManager 错误路径接入不纳入本轮。

## 验证

回归测试覆盖：结束长输出的尾部与显式 cursor、运行中读取部分内容后退出的增量 poll、退出后连续 poll 不重发、截断标记、前台取消与后台 Context 脱钩、启动失败名额释放、并发 Start/Shutdown、并发 poll/stop/list、结束 shell 的孤儿进程、继承 stdout 管道、记录淘汰清理、leader 退出后仍忽略 TERM 的后代、多会话 Shutdown 耗时、实际退出信号。

只读测试通过 Registry 验证 grep 无匹配、非零退出、长输出错误和超时均保留普通结果与截断提示，并使用真实系统隔离执行正常 grep/Git/管道搜索，验证 sed/find/git 参数、重定向、嵌套 shell、rg 预处理器、文件删除/符号链接和工作区外写入均不能修改宿主文件，并用真实监听端点验证 TCP 与 Unix socket 连接被拒绝。Linux 未安装 bubblewrap 时明确跳过系统隔离运行测试；工具本身仍拒绝执行。

本机为 macOS，Linux/Windows 运行时验证需要对应系统。跨平台构建可以验证编译兼容性，不能代替 Linux bubblewrap 的运行验证。
