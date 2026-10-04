# dicom-deident-archive-go

本地 DICOM 影像脱敏、归档、检索、导出与回灌工具。全部处理在本地完成，不依赖任何
外部服务；原始归档文件始终只读、逐字节保存，脱敏只在导出时作用于影像文件的身份
标识元数据，像素数据（含压缩/封装）永不被解码或改动。

## 构建与测试

```bash
go build ./...
go test ./...                           # 单元测试
CGO_ENABLED=1 go test -race -count=3 ./...   # 并发用例竞态检测，多轮稳定
```

## 命令总览

```bash
dicom-archive import   -archive DIR -salt SECRET [-uid-root 2.25] <file-or-dir>...
dicom-archive query    -archive DIR [-salt S] [-patient P] [-modality CT] \
                       [-from YYYYMMDD] [-to YYYYMMDD] [-cursor C] [-limit N]
dicom-archive export   -archive DIR -salt SECRET -out DIR \
                       [-patient P] [-modality M] [-from D] [-to D]
dicom-archive gensample -out DIR [-n 10] [-variants]
```

## 快速复现：导入 → 查询 → 导出 → 回灌

```bash
go build -o dicom-archive ./cmd/dicom-archive

# 1. 样本：三种未压缩语法 + 边缘写法（JPEG 无损/RLE 封装像素、未定义长度序列、
#    定长序列、无文件信息组长度声明）
./dicom-archive gensample -out sample -n 9 -variants

# 2. 导入并建脱敏索引（-salt 是本地密钥，须保密且在一个部署内恒定）
./dicom-archive import -archive arc -salt my-secret sample

# 3. 查询（过滤条件对 query/export 完全一致）
./dicom-archive query -archive arc -modality CT -limit 2

# 4. 按条件导出完整脱敏数据集（命中的实例一个不少）
./dicom-archive export -archive arc -salt my-secret -out delivery -modality CT

# 5. 隔离环境重建：把导出目录原样导入一套全新空归档
#    同一密钥：伪标识/日期完全不变，不二次偏移
./dicom-archive import -archive rebuilt -salt my-secret delivery
./dicom-archive query -archive rebuilt -modality CT
#    对方若坚持换密钥：也能正常入库，只是整体映射成另一套一致的假名
./dicom-archive import -archive rebuilt-other -salt partner-key delivery
```

## 脱敏规则（`deident` 包）

所有映射都是 `(salt, 原始标识)` 的纯函数：

| 字段 | 策略 |
|---|---|
| PatientID (0010,0020) | `P-` + SHA-256 派生的 64 bit（十六进制） |
| PatientName (0010,0010) | `ANON^` + 哈希 |
| Study/Series/SOP/媒体存储 SOP UID | 在 `-uid-root`（默认 `2.25`）下用 120 bit 哈希合成合法 UID |
| 其他 UID 元素（序列内引用 UID、FrameOfReferenceUID 等） | 同一“值空间”按原始值一致映射，跨文件/跨元素保持一致 |
| 所有 DA 日期（StudyDate、PatientBirthDate 等） | 按患者固定偏移（0~364 天，由 `hash(salt, PatientID)` 决定）整体回移；同一患者偏移量相同，先后顺序与间隔天数保持不变 |
| 像素数据（7FE0,0010，含 JPEG/JPEG-LS/JPEG2000/RLE 封装片段） | 一个字节都不改，原样导出 |

文件信息组（0002）会重建：组长度重新计算，`MediaStorageSOPInstanceUID`
跟随 SOP 伪 UID，`TransferSyntaxUID`、SOP Class 等保持不变。序列统一以未定义
长度（Item + 定界符）规范重发；数据集的 VR 与字节序（LE/BE）保持原样。

## 导出语义与清单（`archive.Export`）

导出目录布局：

```
delivery/
├── export-manifest.json
└── <伪PatientID>/<伪StudyUID>/<伪SeriesUID>/<伪SOPUID>.dcm
```

`export-manifest.json` 按导出相对路径排序，逐实例给出：

- `source_path` / `source_sha256`：原归档相对路径与原始字节哈希；
- `export_path` / `export_sha256`：导出相对路径与脱敏文件哈希；
- 伪 PatientID/Name、伪三级 UID、模态、移位后 StudyDate、传输语法；
- `salt_token`：证明脱敏来源的盐派生令牌（不是盐本身）。

保证：

- **完整**：过滤命中的每个实例恰好导出一次；
- **可复现**：相同数据、条件、盐导出两次，文件字节与清单完全一致；
- **幂等**：重复导出到同一目录，内容相同则跳过，不产生副本、不改动已有文件；
- **冲突显式报错**：同相对路径已存在但内容不同 → `ErrExportConflict`，
  不覆盖、不忽略，且本次导出不动目标目录中任何已有内容；
- **半成品不留存**：所有文件先进同目录下隐藏暂存区（`.export-stage-*`），
  成功后逐个 rename 发布；失败自动清理暂存区；
- **原始归档只读**：导出只打开归档文件做读取；
- **并发安全**：导出在一个读锁内取得完整快照，导入提交在写锁内原子完成；
  导出不会看到导入到一半的实例，也不会干扰检索。

### 残留自查

每份文件重写时都会检查输出文件的文本类元素，确认其中不再出现任何原始
PatientID/姓名/三级 UID/日期（按 VM 反斜杠元素精确匹配，避免短 UID 子串误报）。
一旦发现残留，导出中止并返回 `ErrResidualIdentity`，错误信息指出具体文件与
DICOM Tag，绝不静默放过。

## 回灌规则（同密钥 / 换密钥）

导出文件携带两组标记：脱敏来源标记（私有 LO 标签 0011,0010）与盐令牌
（私有 UC 标签 0011,0020），同时写入标准的 `PatientIdentityRemoved
(0012,0062)=YES`。

- **同一盐回灌**：令牌匹配 → 直接采用文件中的伪标识与移位日期，**不再映射、
  不再移位**；索引记录标记 `reimported=true`。新库按原条件查询，命中实例集合
  与原库一致，可直接拿导出清单逐项核对。
- **同一盐、重建库再导出**：重建库再次 `export` 是一次恒等透传——UID（患者、
  研究、序列、实例、媒体存储 SOP、序列内引用 UID 等全部 UID 元素）不再二次
  映射，日期不再二次移位，脱敏标记（provenance/token/`PatientIdentityRemoved`）
  原样保留而不重复插入，因此重建库导出的每份 `.dcm` 与原库交付文件**逐字节
  相同**，`export_sha256` 与患者/研究/序列/实例标识、各级 UID、日期都能和原
  导出清单逐项对上；`source_sha256` 是各归档内源文件的哈希，不同归档不同，属
  预期。重建库重复导出到同一目录仍然幂等、可复现。
- **换盐回灌**：令牌不匹配 → 把文件里的标识当作输入值，用新盐整体再映射成
  另一套一致的假名（同一患者/研究/序列/实例仍然各自一致、日期同患者同偏移），
  正常入库，不报错。

## 支持的写法、传输语法与拒绝条件

接收：

- 隐式 VR Little Endian（`1.2.840.10008.1.2`）；
- 显式 VR Little Endian（`1.2.840.10008.1.2.1`）；
- 显式 VR Big Endian（`1.2.840.10008.1.2.2`）；
- 所有显式 VR LE 的封装（压缩）语法：JPEG 无损（如 `1.2.840.10008.1.2.4.70`）、
  JPEG-LS、JPEG 2000、RLE（`1.2.840.10008.1.2.5`）等，像素不解码；
- 未定义长度的嵌套序列/Item（含动态长度写法）、定长序列、Item 内元素；
- 省略 `(0002,0000)` 文件信息组长度声明的文件；
- 其他结构合法的少见写法（未知但结构完整的元素）不轻易拒收。

拒绝（原因可经 `errors.Is`/`errors.As` 区分，日志含具体文件路径）：

| 原因 | 错误 |
|---|---|
| 缺 128 字节 preamble / DICM | `dicom.ErrNotDICOM` |
| 任意位置截断、声明长度越界、缺 Item/Sequence 定界符、像素片段截断 | `dicom.ErrTruncated` |
| 未知 VR、错位的 Item/定界符等结构错误 | `dicom.ErrCorrupt` |
| Deflated Explicit VR LE（`1.2.840.10008.1.2.1.99`，本地不解压） | `dicom.UnsupportedTransferError` |
| 缺 PatientID/Study/Series/SOP InstanceUID | `dicom.MissingTagError`（指明 Tag） |
| 同 SOP UID 但内容哈希不同 | `archive.ErrConflict`（双方哈希前缀） |
| 导出残留原始标识 | `archive.ErrResidualIdentity`（文件+Tag） |
| 导出目标同名异内容 | `archive.ErrExportConflict`（路径+双方哈希） |

整份文件完整性校验覆盖到 EOF：导入使用全文件验证（`ParseVerify`），以下位置
的截断或损坏都会以可区分的原因拒绝（CLI 打印 `REJECT <文件>` 与具体原因，
归档 API 错误用 `errors.Is/As` 分类并携带源文件路径）：

- **文件信息组**：preamble/DICM、meta 元素头、VR、长度字段、声明组长度未填满；
- **数据集**：任意元素 tag/VR/长度头读到一半、声明值长度超出剩余字节、
  简单元素长度为奇数（像素数据除外）；
- **嵌套序列**：未定义长度序列缺 Sequence Delimiter、未定义 Item 缺 Item
  Delimiter、定长序列/Item 声明区域被底层 EOF 提前截断、定长 Item 内部
  出现 Item/Sequence Delimiter（结构损坏）；
- **像素载荷**（原生）：声明长度只截掉尾部 1~2 字节、载荷中部截断、只剩
  长度声明而整个载荷缺失一律拒绝；声明长度为奇时，流中必须且只能多跟
  1 个 pad 字节，缺 pad 或再多字节都拒绝；
- **像素载荷**（封装：JPEG/JPEG-LS/JPEG2000/RLE）：任意数据片段内部截断、
  片段头/体边界截断、奇数片段长度、缺 Sequence Delimiter 一律拒绝；
- **文件尾部**：像素数据后出现非 `(FFFC,FFFC)` 元素（结构错误）、
  `(FFFC,FFFC)` Data Set Trailing Padding 之后仍有多余字节、尾部杂散字节
  无法构成完整元素头，都拒绝；`(FFFC,FFFC)` 填充本身被正确接受。

同文件重复导入为幂等 `DUP`，不产生副本；同 SOP UID 但内容不同仍以
`ErrConflict` 拒绝。导入与导出都使用 256 KiB 流式缓冲，单文件几百 MB
也不会整份读入内存。

## 本地造样本、复现与验证

`gensample -variants` 产出（可直接用来导入/导出/回灌验证）：

- `jpeg-lossless.dcm` / `rle.dcm`：封装压缩像素（约 300 KB 确定性载荷），
  验证逐字节保留；
- `undef-sequence.dcm`：未定义长度嵌套序列，内含引用 UID，验证序列透传与
  UID 重写；
- `defined-sequence.dcm`：定长序列写法；
- `no-meta-group-length.dcm`：无 `(0002,0000)` 长度声明。

所有 `gensample` 产物（常规轮转样本与全部边缘变体）均按严格 DICOM P10
规范生成，本仓库的 `dicom.ParseVerify` 全文件校验必须全部通过：

- 文件信息组按 Tag 升序排列，含 File Meta Information Version `(0002,0001)`、
  Media Storage SOP Class/Instance UID、Transfer Syntax UID、Implementation
  Class UID/Version Name；组长度字段位置与数值正确；
- 显式 VR 未压缩像素使用 `OW` 长 VR 头（`VR + 2 保留字节 + 32 位长度`），
  不会再出现短头导致严格校验器把载荷前 4 字节误读为声明长度的“长度越界”；
- 数据集带完整常用 IOD 必填元素（Image Type、Study/Series/Instance 标识、
  Manufacturer、像素模块 `(0028,*)`：SamplesPerPixel、PhotometricInterpretation
  MONOCHROME2、Rows/Columns、PixelSpacing、BitsAllocated/Stored、HighBit、
  PixelRepresentation）；封装样本的像素片段长度恒为偶数、BOT + 数据片段 +
  Sequence Delimiter 完整。

能收的写法与传输语法（与下文“接收/拒绝”清单一致）：隐式/显式 LE、显式 BE、
全部显式 VR LE 封装语法（JPEG 基线/扩展/无损、JPEG-LS、JPEG 2000、RLE）、
未定义/定长序列、无 meta 组长度；像素始终不解码、不改写。

手动验证建议：

```bash
# 可复现性：两次导出比较
./dicom-archive export -archive arc -salt k -out d1
./dicom-archive export -archive arc -salt k -out d2
diff -r d1 d2 && echo "byte-identical"

# 幂等：再导一次不应改动/新增文件
find d2 -newer d2/export-manifest.json   # 无输出

# 隔离环境重建 + 重建库再导出（同密钥）：.dcm 必须逐字节一致，
# manifest 除 source_sha256（各归档内源文件哈希）外完全一致
./dicom-archive import -archive rebuilt -salt k d1
./dicom-archive export -archive rebuilt -salt k -out d3
for f in $(cd d1 && find . -name '*.dcm'); do cmp d1/$f d3/$f || echo "DIFF $f"; done
diff <(grep -v source_sha256 d1/export-manifest.json) \
     <(grep -v source_sha256 d3/export-manifest.json) && echo stable

# 完整性：像素尾部 / 载荷内部 / 只留长度声明，全部必须 REJECT
cp sample/sample-000.dcm cut-tail.dcm  && truncate -s -2 cut-tail.dcm
cp sample/jpeg-lossless.dcm cut-frag.dcm && truncate -s -100 cut-frag.dcm
./dicom-archive import -archive check -salt k cut-tail.dcm cut-frag.dcm
# -> 两个 REJECT，原因分别落在 ErrTruncated（payload bytes missing /
#    pixel fragment / Sequence Delimiter），日志含完整文件路径
```

## 测试覆盖

- `dicom`：三种未压缩语法、JPEG 无损/RLE 封装、未定义/定长序列、无组长度、
  各区域截断（preamble/meta/数据集/序列/原生像素载荷内部与“只剩长度声明”、
  封装片段内部与定界符、文件尾部）、奇数长度元素/片段、像素 pad 字节规则、
  `(FFFC,FFFC)` 数据集填充接受与杂散尾部拒绝、定长 Item 内错位定界符、
  结构损坏、不支持的 deflated 语法；重写器对 80 MiB 封装像素逐字节保留、
  残留自查、标记去重（重建库不二次插入）；
- `deident`：映射确定性与无碰撞、日期顺序/间隔保持、重跑不漂移、UID 合法性、
  盐令牌稳定；
- `archive`：损坏按可区分原因拒绝、跨文件标识与日期一致性、重复导入幂等、
  冲突拒绝、组合过滤与稳定分页；导出脱敏与索引一致、压缩像素不变、
  清单核对、重复导出幂等、同名异内容冲突、失败无暂存残留、200 MiB 流式
  导入导出、同密钥回灌不二次偏移且查询集合一致、换密钥得到另一套一致假名、
  **同密钥重建库再导出逐字节一致、清单逐项对得上、二次导出幂等**、
  回灌交付集里像素截断文件被拒；导入+查询+导出并发（`-race -count=3`
  下通过）。
- `cmd/dicom-archive`：`gensample -variants` 的全部样本与导出目录中每份影像
  都通过严格 `ParseVerify`，封装像素跨两跳逐字节保留，重建库再导出全部有效。

所有新增/相关用例的日志都打印“输入（传输语法/截断字节数/文件名）→ 结论
（accepted / rejected 及具体原因）”，`go test -v` 可直接核对。
