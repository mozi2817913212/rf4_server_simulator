package gameserver

import (
	"testing"
)

// gradeRatioGradeOf 用与服务端结算相同的档位线判定口径，把体重折算成档位。
func gradeRatioGradeOf(w int64, val, tro, sup int64) int64 {
	v, t, s := val, tro, sup
	return gradeFromLines(&v, &t, &s, w)
}

// 档位比例开启时，四档出鱼占比应接近控制台默认值（3 / 87.5 / 9 / 0.5）。
func TestGradeRatioSamplingMatchesDefaultPercent(t *testing.T) {
	srv, _ := newDetachTestServer(t)
	const val, tro, sup = int64(1000), int64(5000), int64(8000)
	lines := cachedGradeLines{val: ptrI64(val), trophy: ptrI64(tro), blue: ptrI64(sup)}

	const n = 6000
	count := map[int64]int{}
	for i := 0; i < n; i++ {
		w, ok := srv.sampleWeightByGradeRatio(lines, 1, 1, 0)
		if !ok {
			t.Fatalf("第 %d 次抽样没有结果", i)
		}
		count[gradeRatioGradeOf(w, val, tro, sup)]++
	}
	pct := func(g int64) float64 { return float64(count[g]) * 100 / n }
	t.Logf("档位占比: 未达标 %.1f%% 合格 %.1f%% 奖杯 %.1f%% 蓝冠 %.1f%%",
		pct(1), pct(2), pct(3), pct(4))
	if got := pct(1); got < 0.8 || got > 6 {
		t.Fatalf("未达标占比 %.1f%%，期望约 3%%", got)
	}
	if got := pct(2); got < 83 || got > 92 {
		t.Fatalf("合格占比 %.1f%%，期望约 87.5%%", got)
	}
	if got := pct(3); got < 6 || got > 12 {
		t.Fatalf("奖杯占比 %.1f%%，期望约 9%%", got)
	}
	if got := pct(4); got < 0.05 || got > 2 {
		t.Fatalf("蓝冠占比 %.1f%%，期望约 0.5%%", got)
	}
	if total := pct(1) + pct(2) + pct(3) + pct(4); total < 99.9 || total > 100.1 {
		t.Fatalf("四档合计 %.1f%%，应为 100%%", total)
	}
}

// 控制台改比例后应立即生效，且奖杯份额提高会同步压低合格份额。
func TestGradeRatioSamplingFollowsTunables(t *testing.T) {
	srv, db := newDetachTestServer(t)
	setTunable := func(key, value string) {
		if _, err := db.SQL.Exec(`INSERT INTO tunables(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			t.Fatalf("写 %s: %v", key, err)
		}
	}
	setTunable("grade_pct_below", "5")
	setTunable("grade_pct_qualified", "64")
	setTunable("grade_pct_trophy", "30")
	setTunable("grade_pct_super", "1")
	srv.tunMu.Lock()
	srv.tunCache = nil
	srv.tunMu.Unlock()

	const val, tro, sup = int64(1000), int64(5000), int64(8000)
	lines := cachedGradeLines{val: ptrI64(val), trophy: ptrI64(tro), blue: ptrI64(sup)}
	const n = 4000
	trophy := 0
	for i := 0; i < n; i++ {
		w, ok := srv.sampleWeightByGradeRatio(lines, 1, 1, 0)
		if !ok {
			t.Fatalf("第 %d 次抽样没有结果", i)
		}
		if gradeRatioGradeOf(w, val, tro, sup) == 3 {
			trophy++
		}
	}
	if got := float64(trophy) * 100 / n; got < 25 || got > 35 {
		t.Fatalf("奖杯占比 %.1f%%，配置为 30%% 时应落在 25~35%%", got)
	}
}

// 鱼种没有奖杯线/蓝冠线时只在可用档位里归一化，不得产生不存在的档位。
func TestGradeRatioSamplingWithoutTrophyLines(t *testing.T) {
	srv, _ := newDetachTestServer(t)
	const val = int64(1000)
	lines := cachedGradeLines{val: ptrI64(val)}
	for i := 0; i < 500; i++ {
		w, ok := srv.sampleWeightByGradeRatio(lines, 1, 1, 0)
		if !ok {
			t.Fatalf("第 %d 次抽样没有结果", i)
		}
		if w <= 0 {
			t.Fatalf("体重非法: %d", w)
		}
		if w >= val*6 {
			t.Fatalf("体重 %d 超出无奖杯线时的上界 %d", w, val*6)
		}
	}
}

// 关闭档位比例时返回 (0,false)，由调用方退回对数正态标定。
func TestGradeRatioDisabledReturnsFallback(t *testing.T) {
	srv, db := newDetachTestServer(t)
	if _, err := db.SQL.Exec(`INSERT INTO tunables(key,value) VALUES('grade_ratio_enabled','0')
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		t.Fatalf("写开关: %v", err)
	}
	srv.tunMu.Lock()
	srv.tunCache = nil
	srv.tunMu.Unlock()
	lines := cachedGradeLines{val: ptrI64(1000), trophy: ptrI64(5000), blue: ptrI64(8000)}
	if w, ok := srv.sampleWeightByGradeRatio(lines, 1, 1, 0); ok || w != 0 {
		t.Fatalf("关闭时应返回 (0,false)，实得 (%d,%v)", w, ok)
	}
}

// 装备上限远够不到该鱼种时按不可钓处理，交给上层换鱼种或空竿。
func TestGradeRatioUnreachableTrophyWithGearCap(t *testing.T) {
	srv, _ := newDetachTestServer(t)
	lines := cachedGradeLines{val: ptrI64(1000), trophy: ptrI64(50000), blue: ptrI64(80000)}
	if w, ok := srv.sampleWeightByGradeRatio(lines, 1, 1, 1000); !ok || w != 0 {
		t.Fatalf("上限 1kg 够不到 50kg 奖杯线，应返回 (0,true)，实得 (%d,%v)", w, ok)
	}
}
