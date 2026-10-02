# 本地 DICOM 脱敏、归档与检索（Go，纯离线）

本工程在本地完成 DICOM 影像的**解析、身份标识脱敏、归档与检索**，不依赖任何
外部服务或第三方运行时（仅使用 Go 标准库）。针对来源杂乱、传输语法/VR
编码不一、文件损坏或截断、重复导入、边导入边查询等情况，结果保持
**稳定、可解释、可复现**。

## 1. 能力概览

- **解析**：自带 DICOM PS3.10 解析器，支持以下传输语法与显式/隐式 VR：
  - Explicit VR Little Endian `1.2.840.10008.1.2.1`
  - Implicit VR Little Endian `1.2.840.10008.1.2`
  - Explicit VR Big Endian `1.2.840.10008.1.2.2`
  - Deflated Explicit VR Little Endian `1.2.840.10008.1.2.1.99`（解压后解析）
  - 支持 SQ 序列（定长/不定长、item delimitation）、UN 不定长、
    封装像素数据（fragment，逐字节保留）。
- **拒绝策略（不静默入库）**：每个坏文件都带**稳定的原因码、文件名、标签**：
  `NOT_DICOM_PREAMBLE`、`TRUNCATED_FILE`、`BAD_VR_ENCODING`、
  `UNSUPPORTED_TRANSFER_SYNTAX`、`MISSING_FILE_META`、`MISSING_PATIENT_ID`、
  `MISSING_STUDY_INSTANCE_UID`、`MISSING_SERIES_INSTANCE_UID`、
  `MISSING_SOP_INSTANCE_UID`、`INVALID_DATE_VALUE`。
- **脱敏**：患者标识哈希化、研究/序列/实例 UID 确定性重映射、日期按患者
  整体平移；同名患者及其研究/序列在**整批数据与跨批次重复执行**中映射一致。
- **归档**：原始文件**逐字节**保存（永不改写），脱敏副本单独存储，
  像素数据（`7FE0,0010`）与所有未配置标签原样保留。
- **重复处理**：
  - 内容相同（SHA-256 相同）的重复导入 → **幂等**，不产生第二份副本；
  - 同一 SOP 实例 UID 但内容不同 → 拒绝，原因 `DUPLICATE_INSTANCE_CONFLICT`。
- **检索**：按患者（脱敏后 ID）、模态、日期区间、研究/序列 UID 组合查询，
  固定全局排序 + offset/limit 稳定分页。
- **并发**：导入串行化提交，查询走读写锁快照；提交采用临时文件 + 原子
  rename，查询永远只看到完整实例，不会出现半成品、重复或遗漏。

## 2. 目录与模块

```
cmd/dicomtool/            CLI：import / query / gen-samples / verify / dump-mapping
internal/dicom/           标签字典、解析器（parse.go）、写出器（write.go）
internal/config/          脱敏规则配置（JSON）与内置默认规则
internal/deident/         确定性映射表（mapping.go）与脱敏执行器（transform.go）
internal/archive/         归档、索引、并发导入/查询
internal/sample/          合成 DICOM 样本生成器（含损坏/截断/缺标识变体）
configs/deident.example.json  示例规则文件
```

## 3. 脱敏规则与映射策略

### 3.1 规则动作

配置是一个 JSON 文件（未提供时使用 `internal/config` 内置默认规则）。每条规则：

```json
{ "tag": "(0010,0020)", "action": "hash" }
```

支持的动作：

| 动作 | 含义 |
| --- | --- |
| `keep` | 原样保留（未配置的非身份标签默认行为） |
| `remove` | 删除该元素（支持 SQ 序列整体删除） |
| `empty` | 保留元素但置空值 |
| `replace` | 替换为固定字符串（`"value": "..."`） |
| `hash` | HMAC-SHA256 确定性假名（LO/SH/PN） |
| `hash-uid` | 在组织 UID 根下确定性重映射 UID |
| `shift-date` | 按患者固定的整天偏移平移日期（DA） |

顶层开关：`stripPrivate`（删除所有奇数组私有标签）、
`keepBirthDate`（出生日期是否也平移）、`group:GGGG` 形式的整组删除规则。

### 3.2 标识映射（稳定、无碰撞）

- 患者 ID：`P-` + `HMAC-SHA256(salt | "patient-id", 原始ID)` 前 8 字节十六进制。
- 患者姓名：`ANON^` + 6 字节 HMAC 摘要（与 ID 分开派生）。
- 研究/序列/实例 UID：`HMAC-SHA256(salt | "uid:<scope>", 原UID)` 的
  前 128 位转十进制，拼到 `orgUID` 下，即 `<orgUID>.<digits>`，
  长度控制在 DICOM UID 64 字符以内。
- **同一原始值 → 永远同一新值**；映射表带**反向碰撞检查**，若两个不同原值
  撞到同一新值（截断场景），自动追加区分位。患者、研究、序列、实例
  各自独立命名空间。
- 映射结果持久化在归档目录的 `mapping.json`（敏感文件，建议仅限授权访问），
  每次导入后原子落盘。**重复执行同一批数据直接读回旧映射，不会二次偏移或漂移。**

### 3.3 日期映射（保留患者内先后与间隔）

- 每位患者生成**一个**固定的整天偏移
  `1 … dateShiftDays`，由 `HMAC-SHA256(salt | "date-shift", 患者ID)`
  取模确定；同一患者的 StudyDate/SeriesDate/AcquisitionDate/
  ContentDate（以及默认的出生日期）都加同一个偏移。
- 因此患者内任意两个日期的**先后顺序与天数间隔完全保持不变**；
  不同患者之间偏移互不相关。
- 非法 DA 值（非 `YYYYMMDD`）在解析阶段即以 `INVALID_DATE_VALUE` 拒绝。

## 4. 归档布局与索引约定

对每个归档根目录：

```
<archive>/
  index.json                     可查询目录（原子重写；排序后落盘）
  mapping.json                   原始 → 脱敏 标识/日期/UID 对照（审计用）
  originals/ab/cd/<sha256>.dcm   原始文件，逐字节副本（内容寻址，天然去重）
  studies/<脱敏患者ID>/<脱敏StudyUID>/<脱敏SeriesUID>/<脱敏SOPUID>.dcm
                                 脱敏副本；统一输出为 Explicit VR LE
```

- 原始副本按内容 SHA-256 分桶，**内容相同永远只存一份**。
- 脱敏副本的文件元信息（group 0002）会重写 SOP Instance UID 与传输语法，
  group length 重新计算；像素数据字节不变。
- `index.json` 每条实例记录包含：脱敏患者 ID、模态、脱敏后研究日期、
  三级脱敏 UID、实例号、原始/脱敏相对路径、原始字节数、内容 SHA-256、
  导入时间。

### 并发模型

- 导入由单令牌信号量串行化；导入内部：解析 →（持映射锁）脱敏 →
  写原始文件 → 写脱敏文件 → 在索引写锁内登记并持久化。
- 查询只持有索引读锁复制指针后在锁外排序，因此长时间查询不阻塞导入；
  所有文件写入与 `index.json` 更新均为 `tmp + rename`，读端永远只见
  已提交实例（无半成品、无重复、无遗漏）。

## 5. 检索方式

查询条件可任意组合（空表示不过滤）：

- `--patient` 脱敏后患者 ID
- `--modality` 模态（大小写不敏感，如 CT/MR）
- `--date-from` / `--date-to` 研究日期闭区间（YYYYMMDD）
- 库 API 还支持 `studyUID` / `seriesUID`
- `--offset` / `--limit` 稳定分页；全局排序键为
  `(patientID, studyDate, studyUID, seriesUID, instanceNumber 补零, sopUID)`，
  与索引落盘顺序一致，因此翻页/重跑结果稳定可复现。

## 6. 本地构造样本、复现与验证

需要 Go 1.25+（仓库在 Go 1.26 上验证）。全程离线，无任何网络依赖。

```bash
# 0) 全部测试（含损坏/截断、一致性、幂等、并发竞态）
CGO_ENABLED=1 go test -race ./...
go test ./...

# 1) 生成确定性合成样本（好样本 + 5 类坏样本 + 1 个同 UID 冲突样本）
go run ./cmd/dicomtool gen-samples --out ./tmp/samples

# 2) 导入好样本（也可以直接传目录；支持自定义规则）
go run ./cmd/dicomtool import --archive ./tmp/archive ./tmp/samples/good

# 3) 再次导入同一批 -> 全部 IDEMPOTENT，不产生重复副本
go run ./cmd/dicomtool import --archive ./tmp/archive ./tmp/samples/good

# 4) 坏样本：每个文件按不同原因码拒绝（退出码 3）
go run ./cmd/dicomtool import --archive ./tmp/archive ./tmp/samples/bad

# 5) 同一 SOP UID 不同内容 -> DUPLICATE_INSTANCE_CONFLICT
go run ./cmd/dicomtool import --archive ./tmp/archive ./tmp/samples/conflict

# 6) 组合查询 + 稳定分页
go run ./cmd/dicomtool query  --archive ./tmp/archive --modality CT --limit 2
go run ./cmd/dicomtool query  --archive ./tmp/archive --offset 2 --limit 2

# 7) 校验磁盘文件与索引一致；查看标识/日期对照
go run ./cmd/dicomtool verify --archive ./tmp/archive
go run ./cmd/dicomtool dump-mapping --archive ./tmp/archive

# 8) 使用自定义规则
go run ./cmd/dicomtool import --archive ./tmp/archive2 \
  --config configs/deident.example.json ./tmp/samples/good
```

### 测试覆盖

- `internal/dicom/parse_test.go`：四种传输语法、deflate、损坏/截断/缺标识/
  坏日期，断言**可区分原因码且错误能定位到文件与标签**。
- `internal/deident/deident_test.go`：2000 个原始 ID 无碰撞、跨文件/跨实例
  标识一致、日期偏移恒定且间隔保持、重复加载映射无漂移、像素不变、
  原始姓名不泄漏。
- `internal/archive/import_test.go`：拒绝原因可区分、幂等导入只存一份、
  同 UID 异内容冲突拒绝、原始文件字节一致 + 脱敏文件像素一致、
  组合过滤与稳定分页。
- `internal/archive/concurrent_test.go`：8 导入协程 × 10 实例 +
  12 路对同一文件的并发重复导入 + 持续查询读端，在 `-race` 下断言
  无重复、无遗漏、无半成品（实例已提交时其文件必然存在）。

单测日志对每个判定打印**输入标识与判定依据**（如
`INPUT file=truncated.dcm -> REJECT reason=TRUNCATED_FILE basis=...`、
`INPUT PID-A dates=[...] -> shifted=[...] basis=fixed per-patient HMAC offset`），
运行 `go test -v ./...` 可查看。
