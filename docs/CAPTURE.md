# 本地捕获模式

捕获模式从现有网页侧栏进入。脚本负责准备本机后台服务，网页负责分步安装、权限与证书检查、处理端验证和日常管理；没有原生桌面窗口或托盘。

## 使用流程

1. 点击“捕获模式”，选择处理位置。新安装默认本地。
2. 根据浏览器系统下载安装脚本，执行网页给出的命令。脚本准确检测实际架构，校验发布文件后注册后台服务，并打开本机向导。
3. 点击“授权并检查”，完成操作系统要求的授权。
4. 生成并信任本机 CA，再检查证书状态。每台安装的私钥独立生成，不包含在发布包中。
5. 验证处理端并运行脱敏、还原自检。远程模式需要配置启用了转交功能的 HTTPS Silencer 服务器和管理令牌。
6. 开启捕获，从原来的 AI 客户端发送请求，在处理端账本中确认。启用前已建立的连接可能需要重连。

每一步按实际检查放行。链路检查执行本机 CA 签发证书的真实 TLS 握手，以及处理端脱敏、还原自检。启用操作等待组件确认已应用当前目标列表；组件心跳本身不证明所有应用已经受到保护。独立证书库、证书固定及不可识别的域名仍可能需要单独处理。

macOS 可能允许导入证书，却拒绝后台命令设置信任，提示无法进行用户交互。此时在“钥匙串访问 → 系统”打开本次安装的 `Silencer Local Capture`，展开“信任”，将“使用此证书时”设为“始终信任”，关闭详情并完成系统确认，再回网页检查。后台检查通过前不会开启保护。

网页向导提供打开系统设置与钥匙串、分步操作说明、经本机鉴权的证书下载和 SHA-256 指纹核对。下载仅包含本机 CA 公钥证书；私钥不通过接口导出。后台无法设置信任时会展开手动步骤。点击“检查并继续”后按真实状态放行，不以打开了系统窗口或下载了证书作为完成条件。

页面分别展示本机连接、捕获运行条件和客户端请求验证。重新进入页面后先查询实际状态，旧缓存与断线前通过的检查不能用于开启保护或前进。修复向导保留当前会话的入口与步骤；服务端已有完整配置时，正常进入配置页。捕获运行状态同时核对组件、证书、处理链路、心跳与目标生效情况，失效时显示异常。

### cc-switch 等本机 API 网关

固定版本的 macOS 捕获后端排除回环流量，不能直接捕获发往 `localhost`、`127.0.0.0/8`、`::1` 的请求；配置接口和启用检查会拒绝这些目标。Claude Code 通过 cc-switch 时，应将 cc-switch 实际访问的模型域名加入保护目标，并确认 cc-switch 的 TLS 客户端信任本机 CA。不要把本机 API 反向服务地址填写为 HTTP 出站代理。客户端原有地址和 Key 保持不变，最终仍需在捕获账本中验证实际请求。

如果网关有故障转移或多供应商路由，需要确认所有实际出站域名；向导的链路步骤也可直接保存这些保护目标。新装 CA 后，部分已运行应用需要重启才能加载新的信任库。

该限制来自 [官方 macOS redirector 的网络规则](https://github.com/mitmproxy/mitmproxy_rs/blob/v0.12.10/mitmproxy-macos/redirector/network-extension/TransparentProxyProvider.swift) 与 Apple `NENetworkRule` 对默认规则排除 loopback 的定义；并非修改目标列表即可解决。

## 本地与远程处理

| 内容 | 本地处理 | 远程处理 |
| --- | --- | --- |
| 系统捕获、证书、权限 | 本机 | 本机 |
| 脱敏、还原、工具防护 | 本机 | 指定服务器 |
| 原文日志与映射 | 本机 | 指定服务器 |
| 到模型的出站网络 | 本机配置 | 服务器配置 |

远程模式将原始请求及供应商凭据经 HTTPS 交给指定服务器。本机只转交，不进行第二次脱敏，不重复保存完整业务正文。管理凭据只用于 Silencer 接入，访问模型前剥离。

切换先验证新处理端，只影响新请求。进行中的请求及已提交的配置写入固定原目标；查询缓存和详情状态按处理端隔离。规则、历史日志和映射不自动同步或迁移。远程故障不自动回退本地。

## 管理员：组件发布

捕获组件固定使用 mitmproxy 12.2.3；Linux 同时固定 mitmproxy-rs 与 mitmproxy-linux 0.12.11。Windows/macOS 使用官方独立分发包的完整内容，保留系统助手、签名和许可证。不要只复制 Python 环境中的 mitmdump 启动脚本。Linux 打包工具会执行实际独立运行时的包版本探针；缺少版本元数据或使用旧助手时拒绝打包，捕获插件也会再次检查。Linux 最多使用 20 条进程过滤项，超出后拒绝启用，避免后端静默截断排除规则。

Linux 旧助手曾以线程 ID 匹配进程过滤，可能漏掉工作线程；参见 [上游修复 #314](https://github.com/mitmproxy/mitmproxy_rs/pull/314)。不使用枚举线程的方式规避该限制。

在对应原生系统、C++17/Go/Node/PCRE2 构建环境中执行：

```bash
python3 scripts/build-local-release.py --runtime /path/to/extracted-mitmproxy --out native-release
```

官方 Linux 12.2.3 独立包缺少上述两个 distribution 的版本元数据，不能直接通过检查，也不能补写版本号来放行。Linux 使用以下独立构建流程；要求 Linux x64、CPython 3.13、binutils，以及不高于 Ubuntu 24.04 的 glibc（2.39）：

```bash
python3 scripts/build-linux-capture-runtime.py --out /path/to/new-capture-runtime
python3 scripts/build-local-release.py --runtime /path/to/new-capture-runtime --out native-release --pcre2-license /path/to/PCRE2-LICENCE
```

构建器在临时虚拟环境中，按 `scripts/linux-capture-runtime.lock` 的版本和 SHA-256 安装完整 wheel 依赖，再通过上游 PyInstaller hooks 收集实际 Linux 助手和真实包元数据。产物探针核对版本、内嵌助手哈希与适配插件导入；输出附带依赖锁、安装报告、许可证与构建清单。输出目录必须不存在。探针仅使用临时普通代理模式，不请求捕获权限、不安装 CA 或修改系统网络。Windows/Linux 打包同时检查可执行文件架构；Windows 递归收集实际 DLL 导入依赖，Linux 核对并收集 PCRE2 的实际动态库名称。

打包与解包验证均通过实际运行时执行 `scripts/capture-runtime-check.py`。Linux 检查助手存在、可执行、x64 架构及与构建清单一致的哈希；Windows 检查 redirector、WinDivert DLL 和驱动的存在、架构与哈希。打包时生成 `capture-runtime-manifest.json`，解包测试再次核对实际文件；只报告 mitmproxy 版本或包元数据不能替代这些检查。

输出对应平台的原生包、`manifest.json` 和 `SHA256SUMS`。将这些公开文件放在 HTTPS 发布地址，并在提供安装引导的网关设置：

正式发布前需遵循 [发布流程](PUBLISHING.md) 完成审核和导出，并从已审核的公开源码构建。本机开发测试包不直接公开发布。

```text
SILENCER_CAPTURE_RELEASE_BASE=https://downloads.example.com/silencer
```

发布目录只包含构建产物，不应包含运行配置、数据库、CA、请求正文或 `.capture` 私有目录。构建命令仅为自有 macOS 动态库添加本地 ad-hoc 签名，不进行 Developer ID 签名、公证、上传或系统安装。

macOS 需要保留完整 `mitmproxy.app`，打包器接受该应用或其父目录，并生成隔离动态库环境的启动入口。自有组件的最低部署目标固定为 macOS 14.0；依赖也必须兼容该版本，打包器会拒绝最低系统版本过高的库。可通过 `PKG_CONFIG_PATH` 选择兼容的 PCRE2 构建；`--pcre2-license` 与可重复的 `--license` 参数用于附带相应许可证，包括启用 JIT 时的 SLJIT 许可证。

macOS 分发需符合系统扩展签名与公证要求。首发目标是 Windows 10/11 x64、macOS 14+（Intel/Apple Silicon）、Ubuntu 24.04 x64（内核 6.8+）。WSL 和容器内捕获不属于首版目标。

## 管理员：远程处理端

```text
SILENCER_GATEWAY_TOKEN=<strong-management-token>
SILENCER_CAPTURE_FORWARD_ENABLED=true
SILENCER_CAPTURE_ALLOWED_TARGETS=api.openai.com:443,api.anthropic.com:443
```

通过可信 HTTPS 入口提供服务。本机客户端验证服务器证书，不提供跳过证书校验开关。

远程转交为 `/api/capture/forward`，要求非空 `X-Silencer-Token` 和协议版本。不会借用供应商 Authorization 或控制台 Cookie 进行管理认证。接入服务器必须允许原始目标，不会用全局上游或命名路由替换它。

首版接入令牌是服务器管理令牌，本机服务因而拥有相应管理权限。原始供应商凭据与管理令牌是独立字段。

## 配置与恢复

本机服务必须绑定回环地址，并启用：

```text
SILENCER_LOCAL_MODE=true
SILENCER_GATEWAY_ADDR=127.0.0.1:18711
SILENCER_CONTROL_CONFIG_FILE=<private-data-directory>/control.json
SILENCER_MITMDUMP_PATH=<bundle>/runtime/mitmdump
```

脚本自动设置这些值。Windows 使用登录计划任务，macOS 使用 LaunchAgent，Linux 使用 systemd 用户服务。

Ubuntu 桌面需要 `sudo`、`lsof` 及 `zenity` 或 `kdialog`。Linux 授权通过系统密码窗口完成，不在网页接收或保存密码；后台重启后如系统要求，会再次出现授权窗口。固定适配器只接受 mitmproxy 12.2.3 的授权检查与 redirector 两种 sudo 调用。无桌面授权代理的环境会报告缺失条件，不会宣称授权完成。

Windows 首版安装器仅接受原生 x64 Windows 10/11。计划任务固定登记在根目录，安装、升级、修复与卸载均先核对任务启动程序和本次安装目录；无法枚举任务时阻止维护。Windows 私有捕获状态目录使用仅当前用户与 SYSTEM 可访问的 ACL，并修复已有文件权限；配置替换保留原文件的访问权限。Linux 同样核对 systemd 用户服务文件与已加载服务的启动入口，即使服务文件已丢失，也不会操作另一目录的同名服务。

Windows 系统代理检测按注册表字段分别解析启用状态、静态代理、PAC 与自动发现。未知或自动代理配置需要明确确认；检测不修改系统设置。Linux 缺少依赖、无可用桌面会话、取消授权和内核后端加载失败各自提供诊断。捕获子进程使用独立进程组，停止后继续核对同组辅助进程；无权限或无法确认退出时保留失败状态。这不替代对提权后独立会话辅助进程的实机验收。

Windows 捕获进程先以暂停状态创建，加入启用了关闭时终止成员的 Job Object，再恢复主线程；停止时终止 job 并确认活动进程数为零。相关原生测试覆盖父进程仍运行与父进程先退出两种情况。实现依据 [Windows Job Objects](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects)；原生 Windows/UAC 助手行为仍需实测。

安装与修复始终使用新下载且校验通过的文件，旧缓存仅用于失败恢复。Windows 同时递归修复已有文件的显式权限，并拒绝重解析点。Linux 卸载前要求明确的停止状态、零主/控制进程及无本次安装的运行进程；查询失败时保留安装文件供重试。安装回滚失败时保留恢复目录，不删除恢复材料。

捕获配置及远程凭据独立保存在控制文件旁的 `.capture` 私有目录，不能通过普通规则备份或处理端切换转移。完整日志与持久化映射沿用现有保留规则。

修复、更新任务保存实际进程 PID 与唯一脚本标识。页面恢复时核验对应进程；确认退出的未完成任务显示“已中断，可重试”。无法读取任务记录或核验进程时阻止并发维护；任务存活期间不允许开启捕获或修改本机处理配置。旧任务的完成回调不能覆盖新任务。

- 已进入保护流水线的请求处理失败时阻断。
- 未启用的目标保持原网络路径，不采集正文。
- 用户停止后不再接管新连接。
- 运行时故障由后台监护重试，界面显示真实异常；首版不提供永久断网开关。
- 更新和卸载仅适用于脚本登记的安装。卸载保留配置、CA 文件及业务数据，系统证书信任可在卸载引导中选择清理。

### 可视化卸载

在本机网页的“捕获模式 → 维护 → 卸载引导”进入独立的三步流程：

1. **停止捕获**：执行停止操作并检查实际状态。仍启用或组件仍在运行时，不能下载卸载脚本。
2. **确认清理范围**：默认清理本次安装的系统 CA 信任；也可明确选择保留，供重新安装使用。页面展示证书指纹和各系统的授权、手动处理指引。
3. **卸载与检查**：下载绑定当前安装的脚本，在本机终端执行。脚本包含固定维护代码及 CA 公钥，不包含私钥或管理凭据，也不联网下载组件。先核对证书和服务归属，移除后台服务，再处理证书信任，最后自动打开自包含的本地卸载报告。

报告分别显示服务和证书的真实结果及失败原因。后台服务退出后仍可阅读；网页连接中断、脚本已下载都不会显示为卸载成功。执行开始即覆盖旧报告，中断后保留本轮未完成状态。修复失败原因后可以重复执行；若本机 CA 身份已变化，应重新下载脚本。

浏览器在本机保存最多七天的步骤、清理选择、证书公钥指纹和脚本下载记录，不保存令牌或完成断言。网页能够重新加载时，返回可接续步骤，并重新检查实际状态；服务已移除时直接查看独立报告。打开的页面断线后仍可前后浏览操作说明，下载新脚本需重新连接并确认捕获已停止。发现证书变化时清除旧下载记录，发现更新或重新安装时重置旧向导。

脚本保留配置、CA 文件、日志、映射和安装缓存，远程处理端数据不受影响。未登记的开发实例不会操作系统服务，应从启动它的终端停止。官方 Mitmproxy 共享网络扩展保留，确认无其他用途后可按页面指引停用；应用独立证书库中的手动副本需要在对应应用中清理。

证书检查由保留的网关二进制以只读命令完成，不启动服务、引擎或数据库。检查使用证书有效期内的时间，避免把“已过期但仍受信任”误报为清理成功；证书不匹配、信任库不可用或验证异常均保留为失败。这不会改变业务请求的 TLS 验证行为。

系统代理检测不会修改原有代理、路由和 DNS。HTTP CONNECT 按 authority 判断目标并保留原代理出口；未知 CONNECT 原样转给原代理。显式认证代理、PAC、HTTPS 代理嵌套以及任意 VPN 组合需要单独验收，不宣称全覆盖。

## 验证与发行门槛

```bash
(cd gateway && go test ./...)
python3 scripts/test-capture-native.py
python3 -m unittest discover -s gateway/capture-tests -v
python3 scripts/test-installer-lifecycle.py -v
python3 scripts/test-release-bundle.py -v
npm --prefix frontend run build
npm --prefix frontend run test:capture
npm --prefix frontend run test:logs
```

Windows 额外执行 `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File scripts/test-windows-installer.ps1`，并在 PowerShell 7 下再次执行。该脚本以 fixture 替换计划任务、进程、网络、ACL 和系统身份操作，不登记任务或修改系统权限。

原生 Windows 还执行 `scripts/test-windows-installer-acl.ps1`（临时目录中检查已有敏感文件的显式公共 ACL 修复及重解析点拒绝），以及 `(cd gateway; go test ./internal/platformfs ./internal/hostagent ./internal/gateway -run TestWindows -count=1)`（文件权限、原子替换与 Job Object 子进程退出）。这些用例需在 Windows 执行，交叉编译不等同于运行通过。

生成原生包后，可执行 `python3 scripts/test-local-release.py /path/to/silencer-local-<platform>-<arch>.tar.gz` 检查解包运行、真实原生脱敏还原、目标及凭据分离、组件版本和安装身份。该检查只创建临时文件与回环测试服务，不注册后台服务或启用系统捕获。

原生检查脚本从仓库根运行。测试覆盖严格认证、原始目标、凭据剥离、作用域、失败切换、证书重试、SSE 首帧、真实 mitmdump TLS 桥接及 CONNECT 分片。TLS 集成使用临时 CA 和测试监听器，不修改系统信任、代理或捕获权限。

新增原生 CI 针对 Windows、macOS 和 Linux 构建并执行捕获测试，包含 Windows PowerShell 5.1/7 脚本测试与原生 ACL 测试，以及 Linux 锁定运行时构建、完整分发打包和解包验证。CI 通过仍不能代替系统权限、签名、安装升级卸载和 Clash/TUN 的实机验收。未通过的平台或连接组合不能标为正式支持。

### 当前实现的验证记录

在 macOS 开发环境中已通过 Go 全量测试、原生 C++ 引擎及 Go 测试、捕获并发检测、20 项捕获 Go 测试、15 项适配器协议测试、前端构建、处理端隔离测试和现有日志测试。真实 mitmdump 测试验证临时 CA 下的 TLS 转交；远程流测试验证切换后旧流继续使用原处理端、新请求使用新处理端。网页实测了本地默认值、安装资源提示、权限门禁与刷新恢复。

Unix 安装脚本的 16 项隔离测试覆盖 macOS/Linux 的重复安装、更新、损坏版本修复、下载中断、校验和与归档拒绝、服务及就绪失败回滚、回滚失败时保留恢复文件、卸载保留数据、可视化报告、重复卸载、卸载中断与旧脚本检测证书变化、Linux 服务归属和停止状态检查，以及测试目录防越界。其中逐阶段中断测试在停止服务、删除服务文件、删除启动入口、删除版本链接和删除安装标记后，验证同一下载脚本可继续执行且业务数据保持一致。隔离模式不调用真实系统服务、网络下载或浏览器，也不修改 HOME。

已在本机生成 macOS Apple Silicon 完整测试分发包，保留官方应用签名，并用兼容 macOS 14 部署目标的静态 PCRE2 构建引擎；解包后的网关、运行时及原生处理链路验证通过。本地包尚未进行 Developer ID 签名、公证或公开发布，也不能替代 macOS 14/Intel 实机验收。

Windows 文件锁、私有目录 ACL 与宿主配置替换模块，以及 Job Object 进程树管理（含专用测试）完成交叉编译检查，14 项安装脚本 fixture 测试已在 macOS 的 PowerShell 7 下通过。完整网关依赖 cgo/SQLite 和本机 C++ 工具链，不能用禁用 cgo 的交叉构建替代。Windows 原生 ACL、Job Object、PowerShell 5.1 和新增三平台 CI 尚未远程执行。

尚未完成：Windows/Linux 系统捕获与授权、其他 macOS 版本与架构、Clash 系统代理模式及其他 TUN 组合、系统重启与重新登录后的自启动验收、Windows 完整分发包及正式签名发布。下方记录了当前 Mac 的 TUN 共存及真实后台安装生命周期验收，以及 Linux 完整包的容器验证；这不代表其他系统或代理组合均已覆盖。当前没有公开发布可下载的安装产物，因此未配置发布地址时，尚未安装的网页会明确提示资源未发布；已安装并通过检测的组件可直接继续。

### 2026-10-19：Claude Code 与 cc-switch 实机验证

在 macOS 26.2 Apple Silicon 上，使用官方 mitmproxy 12.2.3、cc-switch 4.0.12 和 Claude Code 2.1.303 进行了真实本地捕获。测试实例使用独立数据目录；Claude Code 禁用工具、持久会话和项目设置读取，仅发送合成邮箱提示词。安装并授权官方网络扩展、手动完成系统 CA 信任后，重启 cc-switch 重新加载证书并建立新连接。

| 核验项目 | 实测结果 |
| --- | --- |
| 原客户端经 cc-switch 访问模型 | 基线与捕获请求均成功 |
| 真实捕获入口 | 同一 Claude 会话仅有一条账本记录，`ingress=capture`、HTTP 200 |
| 请求脱敏 | 原始请求含合成邮箱，上游请求不含邮箱且包含 EMAIL 占位符，`mask_count=1` |
| SSE 还原 | 上游流回显占位符，客户端收到原邮箱，`restored_count=2`、`unresolved_count=0` |
| 中文标点 | 紧邻全角冒号的邮箱正确识别，中文提示前缀保留；逗号、分号另有原生回归测试 |
| 正文与错误 | 账本正文完整，无请求错误 |
| 向导 | 实测已安装组件免重复安装、权限检查后前进、证书手动步骤与指纹展示 |

实测修复了透明模式下 `Request.host` 为 IP 导致域名目标漏匹配的问题。HTTPS 目标从 TLS SNI 恢复，并核验 Host/authority 与实际端口；声明冲突时阻断。还补齐了系统信任命令无法交互时的手动引导，以及中文标点后的邮箱识别边界。

本次测试沿用客户端当时的地址与凭据，没有通过修改客户端地址接入 Silencer 反代。cc-switch 的多供应商路由需要将所有实际出站域名加入捕获目标。模型没有回显占位符时，账本会显示 `no_placeholder_in_response`；请求成功本身不能证明发生了响应还原。

本机其他网络扩展保持启用，未修改系统代理、路由或 DNS；这不替代 Clash/TUN 的专项验收。此次未注册 Silencer 系统后台服务，测试结束后停止捕获，保留本机网页供审阅。系统权限、签名与证书操作仍由操作系统确认。

### 2026-10-20：Clash Verge TUN 共存与后台生命周期

在同一台 macOS Apple Silicon 机器上，通过正在运行的 Mihomo 控制器只读接口确认：Clash Verge 为规则模式，TUN 已启用，使用 System 栈、自动路由与 DNS 接管；系统 HTTP/SOCKS 代理关闭。测试沿用这些设置，未改节点、规则、代理、路由或 DNS。

Claude Code 经原有 cc-switch 网关发送唯一合成邮箱提示词，禁用工具与会话持久化。结果为 HTTP 200，同一合成输入仅有一条 `ingress=capture` 记录：`mask_count=1`、`restored_count=3`、`unresolved_count=0`。请求脱敏后不含原邮箱，上游 SSE 回显占位符，客户端收到原邮箱。Mihomo 同时观察到 Silencer 网关的 TCP 出站经 TUN 进入原代理链；未发现本次请求循环或重复脱敏。前后控制器配置与系统代理状态一致。这项结果覆盖本机当前 TUN 配置；系统代理 CONNECT 和其他组合仍需单独验证。

随后用独立临时数据目录与真实 LaunchAgent，完成以下实机验收：

- 从校验和匹配的完整原生包经 HTTPS 安装，核对运行进程、版本身份与就绪状态。
- 停止和重新启动后台服务；网关退出后由 KeepAlive 恢复。
- 重复安装同一包，以及从已有原生包升级至包含修复的另一完整原生包。
- 从已登记实例下载可视化卸载脚本，选择保留证书后执行，核对独立报告、服务登记、进程和端口释放。
- 在同一数据目录重新安装后再次卸载；配置、捕获设置、认证文件和数据库测试记录均保留。

这次还补齐了 macOS 安装器的服务归属核验：即使标准 plist 已不存在，只要同名运行服务属于另一数据目录，就在安装或卸载前拒绝操作。实机回归加载了专用测试服务，确认其进程未被误停。

安装生命周期测试使用进程内限定的 HTTPS 测试信任，没有修改系统钥匙串；隔离安装实例始终未启用捕获，也未生成捕获 CA。测试服务已清理，原开发实例与捕获状态已恢复。未执行整机重启、注销登录、系统 CA 清理或共享网络扩展卸载。

### 2026-10-20：Windows/Linux 兼容性与分发验证

- Windows：补齐计划任务归属校验、私有目录 ACL、配置替换权限保留及系统代理字段解析。12 项安装脚本 fixture 测试在 macOS 的 PowerShell 7.6.7 下通过，覆盖重复安装、升级、下载校验、就绪失败回滚、外部同名任务拒绝操作、卸载失败重试与业务数据保留。Windows 专用 ACL/配置替换测试已交叉编译；原生 Windows 与 PowerShell 5.1 结果仍待 CI/实机确认。
- Linux：补齐 systemd 服务归属、授权依赖检查、具体启动诊断和停止时的进程组确认。Unix 13 项安装隔离测试、15 项适配器测试通过；Windows/Linux 的架构与传递 DLL 依赖检查另有 2 项 fixture 测试。
- Linux x64 容器（Debian 12/glibc 2.36，由 Apple Silicon 宿主模拟执行 x64）：Go 全量测试与原生 C++ 捕获测试通过。实际构建了锁定依赖的独立运行时，核对 mitmproxy 12.2.3、两个 0.12.11 distribution 与内嵌助手哈希，真实 TLS 插件转交测试通过。TLS 测试同时修正了冻结程序子进程清理，确认测试结束后无遗留运行时进程。
- 从该运行时构建完整 Linux 原生包，解包后网关启动、版本身份、真实 C++ 脱敏还原、原始目标保留与供应商/管理凭据分离检查通过；保护开关保持关闭，没有生成捕获 CA。macOS Apple Silicon 最新完整包亦通过同样的解包验证。

Linux 构建与协议验证未执行容器内系统捕获，也未操作宿主代理、证书信任或后台服务。Windows/Linux 桌面的提权交互、驱动/eBPF、Clash/TUN 共存和真实安装生命周期仍是发行门槛。

### 2026-10-20：兼容性改动自查

独立审查发现并修正了五类缺口：损坏缓存仍被复用、Windows 已有敏感文件的显式权限未修复、Windows 子进程退出无实际确认、Linux 卸载将查询失败视为停止、交付检查未核对实际助手文件。第二轮独立复核未发现新的 P1/P2 问题。

本轮通过 Go 全量与捕获并发测试、Windows 14 项脚本 fixture、Unix 16 项安装 fixture、4 项分发检查，以及真实 Linux 冻结运行时助手探针和篡改拒绝测试。已有 Linux 测试包补入助手清单后，重新封装并通过解包脱敏还原验证；这项测试不代表重新构建了本轮全部 Go 改动。Windows 进程树用例只完成交叉编译，原生测试已加入 CI。

仍需确认提权后离开原进程组/作业的辅助进程行为，包括 Linux sudo 独立会话和 Windows UAC；当前静态审查未证明这些助手会在连接关闭后遗留，也不能将普通子进程测试当作该场景的实机结果。
