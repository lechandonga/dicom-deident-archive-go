# dicom-deident-archive-go

本地 DICOM 影像脱敏、归档、检索、按条件导出与隔离环境重建工具。全部处理在本地完成，
不依赖任何外部服务；原始归档字节永不被改写，导入/导出全程流式（单文件几百 MB
也不会整份读进内存），整份文件做结构完整性校验。

## 构建与测试

```bash
go build ./...
go test ./...                          # 单元/行为测试
CGO_ENABLED=1 go test -race -count=5 ./...   # 并发用例在竞态检测下多轮
go test -v ./...                        # 每条用例打印输入与结论
go test ./archive -run TestLargeFile    # 250 MiB 流式内存边界用例
```

## 快速复现

```bash
go build -o dicom-archive ./cmd/dicom-archive

# 1. 构造样本；-quirks 同时产出封装像素、动态长度 SQ、缺 meta 长度声明的变体
./dicom-archive gensample -out sample -n 15 -quirks

# 2. 导入并脱敏（-salt 是本地密钥，必须保密且长期固定）
./dicom-archive import -archive arc -salt my-secret sample

# 3. 查询
./dicom-archive query -archive arc -modality CT -limit 2

# 4. 按条件导出脱敏数据集（筛选语义与 query 完全一致）
./dicom-archive export -archive arc -salt my-secret -out ext \
    -patient P-xxxx -modality CT -from 20230101 -to 20241231

# 5. 隔离环境重建：把导出目录导入一套全新空归档（同一密钥）
./dicom-archive import -archive rebuilt -salt my-secret ext/data
./dicom-archive query -archive rebuilt -modality CT   # 命中集合与原库一致

# 6. 幂等：重复导入/重复导出都不产生副本、不改坏已有内容
./dicom-archive import -archive arc -salt my-secret sample
./dicom-archive export -archive arc -salt my-secret -out ext -modality CT
```

## 导出（`archive.Export` / `export` 子命令）

- 筛选条件与查询一致：患者伪 ID、模态、研究日期闭区间；命中的实例**全部**导出，
  不分页、不遗漏。
- 导出目录布局：

```
ext/
├── manifest.json      # 清单（确定性序列化）
└── data/
    └── <伪PatientID>/<伪StudyUID>/<伪SeriesUID>/<伪SOPUID>.dcm
```

- 每份导出文件本身即脱敏成品：
  - PatientID/PatientName 与各级 UID（SOP/Study/Series，及文件内出现的
    引用类 UID、序列内 UID）与索引中的伪标识一致，同一患者/研究/序列/实例
    在整批导出中保持一致；
  - 所有日期型字段（StudyDate、PatientBirthDate 及序列中的 DA）统一按
    同一患者的固定偏移平移，顺序和间隔天数保持不变；
  - 像素数据（含 JPEG 无损、JPEG-LS、JPEG 2000、RLE 等压缩/封装形式）
    **一个字节都不改**，包括基本偏移表和每个分片。
- 可复现：相同数据、条件、密钥重复导出，文件内容与 `manifest.json` 逐字节一致。
- 幂等：重跑跳过相同文件，不产生重复副本，也不改动已导出的文件。
- 防覆盖：目标目录已存在同名但内容不一致的文件时，返回
  `archive.ErrExportContent`，不覆盖、不假装成功。
- 半成品安全：导出先在隐藏的 `.export-staging-*` 暂存目录内逐文件生成，
  全部成功才发布；中途失败只清理暂存目录，已发布的文件不受影响。
- 原始归档始终只读，导出只从归档读取。
- 并发安全：导出在一次性一致快照上进行，与导入/查询并发时不会导出半成品实例，
  也不会干扰检索。

### 清单（manifest.json）

每条实例记录：原始归档相对路径 `source_path`、原文件哈希 `source_sha256`、
原始患者/姓名/SOP（`orig_*`，便于对外交付前核对）、伪患者/研究/序列/SOP、
模态、移位后研究日期、**导出文件哈希 `file_sha256`** 以及相对路径
`relative_path`。顶层含 `schema`、脱敏方法版本 `method` 和密钥指纹
`fingerprint`（非密钥本身）。对方拿到目录后可用清单逐文件核对哈希与相对路径。

### 导出残留自查

每份文件重写后立即重新解析，逐字段检查是否还残留：原始患者 ID/姓名、
任一原始层级 UID、任一原始日期值（私有溯源标记 `(0099,1011)` 记录原始
SOP 属例外）。发现残留返回带**具体文件和标签**的
`archive.ErrResidualIdentity`，清单不发布。

## 回灌与隔离环境重建

导出文件带脱敏标记：私有块 `(0099,10xx)`（creator `GSB-DEIDENT`）记录
密钥指纹、方法版本和原始 SOP UID，同时写入标准的
`PatientIdentityRemoved=YES` 与 DeidentificationMethod。

- **同一密钥**重建：新归档识别到匹配指纹，伪标识和日期直接采用，**绝不
  再次移位或再次脱敏**；用原查询条件查新库，命中集合与原库一致；从新库
  再次导出的文件与原导出逐字节相同（见 `TestReimportSameKeyNoSecondShift`）。
- **对方坚持换密钥**：同样正常入库；文件里已有的伪标识被视作一套新的
  “原始身份”，按新密钥派生出另一套内部一致的假名/日期，不报错、不混乱，
  跨文件仍保持同一患者归并（见 `TestReimportDifferentKeyConsistent`）。

## 能收与仍然拒收

可正常入库、检索、导出：

- 隐式 VR LE、显式 VR LE、显式 VR BE；
- 任意压缩/封装传输语法：JPEG 无损（1.2.840.10008.1.2.70）、RLE 无损
  （1.2.840.10008.1.2.5）、JPEG Baseline（…1.2.4.50）等，像素不解码、
  原样收下、原样导出；
- 嵌套序列动态（undefined）长度、动态长度 item、缺
  `(0002,0000)` 长度声明或声明值不规范；
- 少见但结构合法的写法（不常见 VR、`UN` 承载的序列等），尽量宽容接收。

仍然按**可区分原因**拒绝（日志含具体文件路径，`errors.Is/As` 可判定）：

| 情况 | 错误 |
|---|---|
| 非 DICOM（缺 128 字节 preamble / DICM） | `dicom.ErrNotDICOM` |
| 截断、元素长度越界、item/序列未闭合、封装分片损坏、尾随垃圾 | `dicom.ErrTruncated` |
| 结构损坏（非法定界符、非序列元素用动态长度等） | `dicom.ErrCorrupt` |
| 关键标识缺失（PatientID/Study/Series/SOP） | `dicom.MissingTagError`（含标签） |
| 同 SOP UID 但内容不同 | `archive.ErrConflict`（含双方哈希前缀） |
| 同 SOP UID 且内容相同 | 幂等跳过（`DUP`），无副本 |

完整性校验覆盖整份文件：无论截断/损坏发生在文件头、元数据、序列还是
像素分片/文件尾部，都不会放进来。

## 脱敏规则（`deident` 包）

所有映射都是 `(salt, 原始标识)` 的纯函数：

| 字段 | 策略 |
|---|---|
| PatientID | `P-` + 哈希前 64 bit |
| PatientName | `ANON^` + 哈希 |
| Study/Series/SOP 及引用类 UID | 在 `-uid-root`（默认 `2.25`）下用哈希合成合法 UID |
| 日期（含出生日期、序列内 DA） | 按患者固定偏移（0~364 天，由 `hash(salt, PatientID)` 决定）整体平移；顺序、间隔不变 |
| 像素数据 | 永不触碰 |

同一患者所有日期使用同一偏移；映射只从原始值计算，重复执行不漂移。
更换 `-salt` 会整体更换假名；每个部署应固定同一 salt。

## 本地造样本、复现与验证

- `gensample -quirks` 生成混合传输语法及三类少见写法的样本；
  `dicomtest.BuildWithOptions` 可构造：`EncapsulatedPixel`（基本偏移表 +
  分片）、`UndefinedSQ`（动态长度序列/item）、`NoMetaGroupLength`、
  自定义 `PixelPayload`；
- 截断/损坏验证：`TestRejectsCorruptionEverywhere`（每 1/12 长度截断、
  篡改封装分片长度、尾随垃圾）、`TestImportRejectsCorruptionAtAllPositions`
  （归档入口逐位置截断 + 缺关键标识）；
- 像素逐字节：`TestEncapsulatedPixelsByteIdentical`、
  `TestUncompressedPixelsByteIdentical`、`TestExportPixelsByteIdentical`；
- 脱敏一致性与无残留：`TestExportIdentityMatchesIndexAndConsistent`；
- 回灌不二次偏移、换密钥一致：`TestReimportSameKeyNoSecondShift`、
  `TestReimportDifferentKeyConsistent`、`TestReimportIdempotent`；
- 幂等/冲突/失败清理：`TestExportReproducibleAndIdempotent`、
  `TestExportConflictOnTamperedTarget`、`TestExportFailsCleanlyMidRun`；
- 并发：`TestConcurrentExportImportQuery`（`-race -count=N` 稳定）；
- 大文件流式：`TestLargeFileStaysBoundedInMemory`（250 MiB，堆增长接近 0）。

## 归档布局

```
arc/
├── index.json   # 脱敏索引（原子写：tmp + rename）
└── data/<伪PatientID>/<伪StudyUID>/<伪SeriesUID>/<伪SOPUID>.dcm   # 原始字节
```
