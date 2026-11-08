# 鱼获档位比例

Status: done

## Problem Statement

鱼获的未达标、合格、奖杯和蓝冠占比只能通过鱼重倍率间接影响；调整鱼重会同时改变实际重量和经济结果，运营无法稳定配置目标档位比例。若直接把比例功能默认开启，还会在升级后静默改变所有玩家的既有出货分布。

## Solution

后台新增可选的鱼获档位比例模式。管理员显式开启后，可直接配置四档百分比；系统先选档位，再在该档位重量区间内采样。抽样和最终判档统一使用 `settleStatics` 的同一组档位线，缺档位、装备上限和饵效偏移都在同一套规则内处理。默认关闭，关闭时完整保留原对数正态标定。

## User Stories

1. 作为运营，我希望显式开启后按配置比例产出未达标、合格、奖杯和蓝冠鱼。
2. 作为玩家，我希望升级后默认出货分布不变，除非运营主动开启新模式。
3. 边界：鱼种缺少奖杯/蓝冠线时，只对实际存在的档位重新归一化。
4. 边界：装备上限够不到的档位不会产出；完全够不到奖杯线时按当前钓组不可钓处理。
5. 边界：最终判档必须与抽样使用同一组档位线，配置比例不能在结算时失真。

## Implementation Decisions

- `internal/gameserver/fishing.go`：`sampleWeightDB` 在比例模式开启时，以 `cachedSettleStatics` 的 `[0..2]` 作为档位线；关闭时继续使用原对数正态路径。
- 比例参数为 `grade_ratio_enabled`、`grade_pct_below`、`grade_pct_qualified`、`grade_pct_trophy`、`grade_pct_super`；开关代码与后台默认值均为 0。
- 饵效倍率只把部分“合格”份额移给“奖杯”，不直接改变蓝冠绝对占比。
- `internal/admin/api.go`：`grade_ratio_enabled` 纳入 0/1 开关校验。
- `internal/admin/web/index.html`：新增「鱼获档位比例」参数组，并明确默认关闭。
- 数据与迁移：不新增表或迁移，使用现有 tunables。
- 兼容：不新增协议字段；回滚只需把 `grade_ratio_enabled` 设为 0，立即恢复旧标定。

## Testing Decisions

- 单元/行为测试：`internal/gameserver/fishing_grade_ratio_test.go` 覆盖默认比例、参数即时生效、缺档位归一化、默认关闭、显式关闭回退和装备上限。
- 回归测试：构造 `fish_grade_lines.qualified_g` 与 `settleStatics[0]` 不一致的鱼种，调用真实 `sampleWeightDB`，再用最终 `rollGradeForRoll` 判档，确认不会出现由错误抽样线导致的未达标鱼。
- 后台 API 测试：`internal/admin/fishing_rules_test.go` 验证 `grade_ratio_enabled` 仅接受 0/1，且重启后保留。
- 期望值来自固定档位线、显式比例和手算区间，不复用生产随机结果。
- 回滚后变红：默认关闭、同一真源、后台开关校验用例。

## Out of Scope

- 不调整单鱼经验、银价或鱼价公式。
- 不改变关闭状态下的原对数正态分布。
- 不新增客户端协议或 UI 弹窗。
- 不自动把现有玩家配置迁移到比例模式。

## Further Notes

- `[事实]` `fish_grade_lines.qualified_g` 与结算展示的 Record 重量并不总是相同；最终判档一直使用 `settleStatics`。
- `[事实]` 原实现用 `cachedGradeLines` 抽样、用 `cachedSettleStatics` 判档，导致大量鱼种的未达标占比偏离配置。
- `[推断]` 默认关闭是兼容旧经济分布的最小风险方案。
