# aliyundrive

面向 AI Agent 和脚本的阿里云盘 CLI。独立重写，不修改 `../src/aliyundrive-cli`，不依赖交互式终端，也不需要 MCP。

## 设计约定

- 一条命令完成一次操作，默认输出 JSON；成功、错误、帮助使用同一信封格式。
- 不读取交互式输入，不隐式扫码。未登录立即返回 `AUTH_REQUIRED`。
- 没有 `cd` 或隐含工作目录。远端使用绝对路径、`root` 或 `id:FILE_ID`。
- 列表保留文件 ID 和分页游标；重名路径不猜测，改用 ID 定位。
- 所有文件写操作支持 `--dry-run`；回收站操作额外要求 `--yes`。
- 同名上传、重命名、移动默认拒绝冲突；本地下载默认不覆盖。
- 上传、下载同步完成后才返回成功，不在后台偷偷传输。
- 凭据独立保存到仓库外，目录权限 `0700`、文件权限 `0600`，不在输出中返回 token。

## 构建

需要 Go 1.23 或更新版本。

```sh
cd ~/devspace/aliyundrive-cli
make build
./bin/aliyundrive schema --pretty
make install
```

`make install` 将新命令 `aliyundrive` 放进 Go 的 bin 目录，不替换旧命令 `aliyundrive-cli`。项目仅依赖一个 QR PNG 编码库，HTTP、JSON、参数解析和文件传输使用 Go 标准库。

## 登录

可以明确导入旧客户端凭据，不会自动读取或修改旧客户端文件：

```sh
aliyundrive auth import --token-file ~/.go-aliyundrive-sdk/token.json
aliyundrive auth status --check
```

也可以扫码。第一条命令立即返回二维码内容与本地 PNG 路径；打开 PNG，用阿里云盘 App 扫码并确认，然后查询状态：

```sh
aliyundrive auth login
aliyundrive auth poll --wait --timeout 3m
aliyundrive auth status --check
```

`auth poll` 默认只查一次，`--wait` 才轮询。请读取 `data.status`：`pending`、`scanned` 表示尚未完成，`authenticated` 才表示登录成功；`expired`、`canceled` 是终态，不等于登录成功。过期后重新执行 `auth login`。

其他导入方式：

```sh
aliyundrive auth import --stdin < /secure/path/refresh-token.txt
aliyundrive auth logout
```

输入支持纯 refresh token 或旧 SDK 的 token JSON。不要将令牌直接拼到命令参数中，避免出现在进程列表和 shell 历史里。`ALIYUNDRIVE_REFRESH_TOKEN` 也可提供本次调用的凭据，刷新后的凭据会保存到配置目录。

显式登录、导入和退出登录不会因为旧凭据损坏而无法执行。`auth logout` 只清理本地文件，不撤销服务器会话；如果设置了 `ALIYUNDRIVE_REFRESH_TOKEN`，还需要取消该环境变量，后续命令才不会再次使用它。

默认配置目录是 `$XDG_CONFIG_HOME/aliyundrive`，未设置时为 `~/.config/aliyundrive`。可在任意命令使用 `--config-dir DIR` 隔离账号；使用 `--drive-id ID` 明确选择其他盘。`auth status` 只检查本地凭据是否存在，`auth status --check` 才在线验证。

## 常用命令

```sh
aliyundrive ls / --all
aliyundrive ls '/我的文档' --limit 50
aliyundrive ls id:FOLDER_ID --marker 'PREVIOUS_NEXT_MARKER'
aliyundrive stat id:FILE_ID
aliyundrive search '报告' --all

aliyundrive mkdir /Backups/2026 --parents --dry-run
aliyundrive mkdir /Backups/2026 --parents
aliyundrive rename id:FILE_ID '报告.pdf' --dry-run
aliyundrive mv id:FILE_ID /Archive --dry-run
aliyundrive trash id:FILE_ID --dry-run
aliyundrive trash id:FILE_ID --yes

aliyundrive upload ./report.pdf /Documents --dry-run
aliyundrive upload ./report.pdf /Documents
aliyundrive download id:FILE_ID ./report.pdf --dry-run
aliyundrive download id:FILE_ID ./report.pdf
```

带空格的路径需要 shell 引号。上传参数的 `~` 应交给 shell 展开，例如 `~/Downloads/report.pdf`，不要写成 `'~/Downloads/report.pdf'`。以 `-` 开头的文件名放在 `--` 后。首版只支持单文件上传、下载，不支持目录递归传输、断点续传、分享转存、投屏或永久删除。

`--dry-run` 可以读取元数据、联网验证或刷新已有凭据，但不修改网盘、不创建目标目录或传输文件。预览不锁定远端，执行前状态仍可能变化；实际写入时继续拒绝同名冲突。文件操作全程没有确认提示，脚本应检查退出码和 `ok`，不要只匹配日志文本。

## AI 调用契约

```sh
aliyundrive schema
aliyundrive help upload
aliyundrive auth --help
aliyundrive version
```

所有调用（包括参数错误）向 stdout 输出一个 JSON 对象，不混入日志、进度条、ANSI 控制符或二维码文本。`--pretty` 仅改变缩进。

```json
{"schema_version":"1","ok":true,"command":"ls","data":{"items":[],"next_marker":""}}
```

```json
{"schema_version":"1","ok":false,"command":"trash","error":{"code":"CONFIRMATION_REQUIRED","message":"trash requires --yes or --dry-run.","hint":"Preview the item with --dry-run, then explicitly confirm with --yes.","retryable":false}}
```

推荐 Agent 流程：`schema` 发现命令 → `auth status --check` 验证登录 → `ls/search` 获取文件 ID → `stat` 确认目标 → 写操作先 `--dry-run` → 用户确认目标后执行。使用 JSON 字段，不解析表格；多页结果继续传 `next_marker` 或显式指定 `--all`。

| 退出码 | 含义 |
| --- | --- |
| 0 | 命令成功；登录轮询还需检查 `data.status` |
| 2 | 参数错误 |
| 3 | 未登录或认证失败 |
| 4 | 文件或目录不存在 |
| 5 | 冲突或缺少显式确认 |
| 6 | 网络或服务端错误 |
| 7 | 本地 I/O 或配置错误 |
| 8 | 取消或整体超时 |

`--timeout` 默认 `5m`，是整个命令的时间上限；大文件传输可显式增加，例如 `--timeout 1h`。错误中的 `retryable` 表示故障可能是临时的，不保证重试写入安全。传输或写入中断后应先检查远端状态，再决定是否重试。

多级建目录可能部分成功：后续步骤失败时，`error.details.created` 保留已经创建的目录。上传失败且已获得远端 ID 时，`error.details.remote_file` 返回该文件信息，是否清理成功请结合 `error.hint` 检查。上传完成请求的结果不确定时不会自动删除文件，超时或取消后也不会继续发起清理请求。

## 测试与兼容性

```sh
go test ./...
go test -race ./...
go vet ./...
```

自动化测试覆盖全部 17 个命令的调用链，并运行真实可执行文件验证 JSON、退出码、stdin 超时和 SIGTERM 取消。

2026-10-22 已完成真实账号扫码登录与端到端验收：令牌导入、在线状态、文件管理、分页搜索、dry-run、冲突保护、空文件及 11 MiB 分片上传下载均通过。上传下载内容经过 SHA-256 校验，所有测试目录已移入回收站，未修改已有文件。

真实账号的完整测试需要显式开启写操作。先在单独的配置目录完成登录，再执行：

```sh
ALIYUNDRIVE_LIVE_TEST_CONFIG=/path/to/test-config \
ALIYUNDRIVE_LIVE_ALLOW_WRITE=1 \
go test ./internal/cli -run '^TestLiveWorkflow$' -v -count=1 -timeout 12m
```

该测试只修改随机生成的 `aliyundrive-cli-test-*` 目录，覆盖认证、分页、搜索、建目录、重命名、移动、回收站、所有文件写操作的 dry-run、同名及覆盖保护、空文件与 11 MiB 分片上传下载，并校验 SHA-256。结束时把测试目录移入回收站，不永久删除。测试输出不包含令牌、真实文件列表或签名 URL；配置目录内的凭据仍需按敏感文件保护。未设置上述两个环境变量时，普通测试不会联网操作真实网盘。

默认测试使用本地 mock HTTP 服务，不需要真实账号，也不会操作真实网盘。实现参考旧 SDK 的 web API 协议；这不是阿里云盘已注册应用的 Open Platform OAuth 客户端。上游可能要求更新的签名、设备或应用验证，不能将本地测试通过等同于线上可用。不要在本项目中提交凭据或真实账号数据。
