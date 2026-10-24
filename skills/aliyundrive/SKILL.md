---
name: aliyundrive
description: 使用 aliyundrive CLI 操作阿里云盘。当用户要求登录或检查账号、查找和列出网盘文件、建目录、重命名、移动、移入回收站，以及上传或下载单个文件时使用。不是开发 aliyundrive-cli 源码的指南；不支持分享转存、目录递归传输或永久删除。
---

# 阿里云盘 CLI 工作流

使用 `aliyundrive`，不是旧命令 `aliyundrive-cli`。通过 CLI 完成操作，不另写 HTTP 客户端，也不直接修改凭据文件。

## 确认工具和账号

```bash
command -v aliyundrive
aliyundrive version
aliyundrive schema
aliyundrive help upload
```

- 参数以当前二进制的 `schema` 和 `help` 为准；两者不访问账号。CLI 不存在时，若已知本项目源码位置，可在项目根目录运行 `make build`，之后使用 `./bin/aliyundrive`；否则请用户提供安装或源码位置，不猜路径。
- 命令不需要 `--json`：成功、失败、帮助均向 stdout 输出一个 JSON 信封 `{schema_version, ok, command, data?, error?}`。同时检查退出码和 `ok`，从 `data` 取结果；不按日志文字判断。
- 默认凭据目录为 `$XDG_CONFIG_HOME/aliyundrive` 或 `~/.config/aliyundrive`。用户指定账号目录时，每条命令保持相同的 `--config-dir DIR`；需要选择特定盘时保持相同的 `--drive-id ID`。
- `ALIYUNDRIVE_REFRESH_TOKEN` 会优先于配置目录中的 token。切换账号前确认它是否存在，不输出其值，不自行借用其他账号凭据；仅切换 `--config-dir` 不保证账号已隔离。

```bash
aliyundrive auth status --check
```

`auth status` 不带 `--check` 只看本地凭据状态，不能证明在线可用。未登录时停止文件操作并说明需要认证，不因文件命令失败而自动扫码、导入或切换账号。

## 显式登录与退出

仅在用户要求登录或同意认证后选择一种方式。

扫码登录：

```bash
aliyundrive auth login
aliyundrive auth poll --wait --timeout 3m
aliyundrive auth status --check
```

第一条返回 `data.qr_code_png` 和 `data.qr_code_url`；将本地 PNG 路径提供给用户，用阿里云盘 App 扫码并确认，再轮询同一配置目录。`auth poll` 默认只查一次，`--wait` 才持续轮询。

- 只有 `data.status` 为 `authenticated` 才算登录完成。
- `pending`、`scanned` 仍在等待；`expired`、`canceled` 是终态，即使退出码为 0 也不是登录成功。超时或终态后报告状态，不无限重开二维码。

用户已提供凭据文件时，可选择以下其中一条导入，而非要求用户在聊天中粘贴 token：

```bash
aliyundrive auth import --token-file /secure/path/token.json
aliyundrive auth import --stdin < /secure/path/refresh-token.txt
```

输入支持纯 refresh token 或旧 SDK token JSON。不要将 token 拼进参数、打印环境变量值或读取凭据内容到工具输出。凭据存仓库外，目录权限 `0700`、文件权限 `0600`。

退出仅在用户要求时运行 `aliyundrive auth logout`；它只清理本地凭据和二维码，不撤销服务器会话。如果设置了 `ALIYUNDRIVE_REFRESH_TOKEN`，还需由用户移除相应环境配置，否则后续命令仍会使用它。

## 定位文件与分页

```bash
aliyundrive ls / --limit 50
aliyundrive ls id:FOLDER_ID --marker 'PREVIOUS_NEXT_MARKER'
aliyundrive search '报告' --all
aliyundrive stat id:FILE_ID
```

- 大写 ID、游标及示例路径是占位值，执行前换成用户目标或 CLI 返回值。
- 远端没有 `cd`：使用绝对路径、`root` 或 `id:FILE_ID`。`mkdir` 例外，只接受待创建的绝对路径。
- `ls`、`search` 返回 `data.items`，文件 ID 为 `file_id`。重名时不取第一条；结合父目录、类型、大小等核对，优先用 ID，并在写入前 `stat` 确认。
- `search` 搜索选定盘中的名称，不是全文搜索，也不接受自定义查询表达式。
- 默认一页 100 条，`--limit` 为 `1..100`。`data.next_marker` 非空就还没结束；原样传给同一查询的 `--marker`，或按需求使用 `--all`。不要将第一页当作完整结果。
- 对空格和 shell 特殊字符正确引用；以 `-` 开头的位置参数放在 `--` 后。本地 `~` 需由 shell 展开，不写成 `'~/file.pdf'`。

## 文件写入与传输

先明确目标、影响范围与用户授权，再预览。以下均是预览示例，不是一次性执行的任务列表：

```bash
aliyundrive mkdir /Backups/2026 --parents --dry-run
aliyundrive rename id:FILE_ID '报告.pdf' --dry-run
aliyundrive mv id:FILE_ID /Archive --dry-run
aliyundrive trash id:FILE_ID --dry-run
aliyundrive upload ./report.pdf /Documents --name '报告.pdf' --dry-run
aliyundrive download id:FILE_ID ./report.pdf --dry-run
```

- 对照返回计划确认同一账号、盘、源文件、目标位置和名称。已获得用户对该目标操作的明确授权且计划一致时，去掉 `--dry-run` 执行；目标含糊或出现额外影响时先澄清。
- `--dry-run` 不修改网盘、不创建目标目录或传输文件，但可联网读取元数据、刷新并保存已有凭据；它不是离线模拟，也不锁定远端状态。
- `mkdir --parents` 可创建缺失祖先并接受已存在目录。`rename` 的新名称不能包含路径分隔符；`mv` 的目标必须是已存在目录，不隐式改名或覆盖。
- 上传、重命名、移动默认拒绝同名冲突。不要自动改名、先删同名文件或把冲突当作成功。
- 移入回收站实际执行必须用 `aliyundrive trash id:FILE_ID --yes`。`--yes` 只是 CLI 确认参数，不代替用户授权；目录操作影响其内容。不支持永久删除或恢复回收站。
- 上传、下载仅支持单个文件，不支持目录递归或断点续传。上传第二个位置参数是已存在的远端目录；可用 `--name` 指定文件名。下载第二个位置参数是本地文件路径，父目录必须存在。
- 本地下载默认不覆盖。只有用户明确允许替换该本地文件时才加 `--overwrite`，预览时也带上它。
- 上传、下载同步完成后才返回成功；默认 `--timeout 5m` 是整个命令时限，大文件可按需提高，例如 `--timeout 1h`。不要把同步传输语义套到所有写操作。
- 写入后用 `stat` 或父目录 `ls` 核对；下载检查返回的 `local_path` 和 `bytes`。若回收站响应含 `async_task_id`，先说明请求已受理，需要时通过原 ID 核查，不宣称异步任务已经完成。

## 失败与部分成功

读取 `error.code`、`message`、`hint`、`retryable` 和可选 `details`。退出码含义可从 `schema` 获取。

- 认证错误：确认账号上下文并请求显式认证；不要反复尝试旧凭据。参数错误先查帮助；不存在或冲突时重新定位，不擅自扩大操作范围。
- `retryable: true` 只表示故障可能临时发生，不代表重放写入安全。超时、取消或网络失败后，先核查远端和本地状态，避免重复上传、重复建目录或重复移动。
- 多级建目录失败时，`error.details.created` 列出已创建目录；上传失败时，`error.details.remote_file` 可指出已产生的远端文件。结合 `hint` 核查是否已清理，不自动回收、覆盖或删除残留。
- 本地 mock 测试不代表当前真实账号可用；上游兼容性问题如实报告。不绕过 CLI 拼接私有 API，也不自动启用需要真实账号写权限的 live 测试。

向用户简要报告实际完成的操作、必要的文件 ID/路径，以及未完成、异步或结果不确定的部分；不展示 token、签名 URL 或无关私人文件列表。
