# dicom-deident-archive-go

本地 DICOM 影像脱敏与归档检索工具。全部处理在本地完成，不依赖任何外部服务；
原始影像字节永不被改写，脱敏只作用于索引中的身份标识类元数据。

## 构建与测试

```bash
go build ./...
go test ./...                 # 单元测试
CGO_ENABLED=1 go test -race ./...   # 含并发用例，竞态检测下运行
```

## 快速复现

```bash
go build -o dicom-archive ./cmd/dicom-archive

# 1. 构造本地样本（混合三种传输语法：隐式 VR LE / 显式 VR LE / 显式 VR BE）
./dicom-archive gensample -out sample -n 9

# 2. 导入并脱敏（-salt 为本地密钥，须保密且恒定）
./dicom-archive import -archive arc -salt my-secret sample

# 3. 组合查询 + 稳定分页
./dicom-archive query -archive arc -modality CT -limit 2
./dicom-archive query -archive arc -patient P-xxxx -from 20230101 -to 20241231 -cursor 2

# 4. 验证幂等：重复导入同一批文件，全部判定为 DUP，不产生副本
./dicom-archive import -archive arc -salt my-secret sample
```

## 脱敏规则与映射策略（`deident` 包）

所有映射都是 `(salt, 原始标识)` 的纯函数，因此：

- **稳定一致**：同一患者/研究/序列/实例在整批处理及任意次重跑中映射到同一新标识；
- **无碰撞**：标识经 SHA-256 派生，不同原始标识不会碰撞（密码学意义）；
- **不漂移**：映射只从原始值计算，重复执行不会二次偏移。

| 字段 | 策略 |
|---|---|
| PatientID | `P-` + HMAC 风格哈希前 64 bit（十六进制），如 `P-0c78e3da0b05a2f6` |
| PatientName | `ANON^` + 哈希 |
| Study/Series/SOP InstanceUID | 在 `-uid-root`（默认 `2.25`）下用 120 bit 哈希合成合法 UID |
| StudyDate 等日期 | 按患者固定偏移（0~364 天，由 `hash(salt, PatientID)` 决定）整体回移；同一患者所有日期偏移量相同，**先后与间隔关系保留** |
| 像素数据及文件字节 | 不触碰，原样归档 |

更换 `-salt` 会整体更换全部假名，请为一个部署固定使用同一 salt。

## 归档布局与索引约定（`archive` 包）

```
arc/
├── index.json          # 脱敏后的索引（原子写：tmp + rename）
└── data/
    └── <伪PatientID>/<伪StudyUID>/<伪SeriesUID>/<伪SOPUID>.dcm   # 原始字节逐字拷贝
```

- 索引记录含伪标识、模态、移位后日期、原始文件 SHA-256、归档相对路径；
- 全局稳定排序键：`(StudyDate, StudyUID, SeriesNumber, InstanceNumber, SOPUID)`；
- 分页游标为排序结果中的偏移量，配合 `Limit`（默认 50，上限 500）使用；
- 导入与查询并发安全：索引提交在写锁内原子完成，读者只见到完整记录或看不到，
  不会出现半成品、重复或遗漏。

## 拒绝与幂等语义

拒绝原因可区分（`errors.Is` 可判定），日志中包含具体文件路径：

| 原因 | 错误 |
|---|---|
| 非 DICOM（缺 preamble/DICM） | `dicom.ErrNotDICOM` |
| 文件截断 / 长度损坏 | `dicom.ErrTruncated` |
| 不支持的传输语法 | `dicom.UnsupportedTransferError`（含 UID） |
| 关键标识缺失 | `dicom.MissingTagError`（含具体 Tag） |
| 同 SOP UID 但内容不同 | `archive.ErrConflict`（含双方哈希前缀） |
| 同 SOP UID 且内容相同 | 幂等跳过（`DUP`），不产生副本 |

支持的传输语法：隐式 VR Little Endian、显式 VR Little Endian、显式 VR Big Endian。

## 测试覆盖

- `dicom`：三种传输语法解析、非 DICOM、截断、缺关键 Tag、不支持的传输语法；
- `deident`：映射确定性与无碰撞、日期顺序/间隔保留、重跑不漂移、UID 合法性；
- `archive`：损坏/截断文件按可区分原因拒绝并打印判定依据、跨文件标识与日期一致性、
  重复导入幂等、冲突实例拒绝、组合过滤与稳定分页、并发导入+查询（`-race` 下通过）。
