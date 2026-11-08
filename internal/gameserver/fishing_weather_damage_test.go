package gameserver

import (
	"rf4server/internal/store"
	"testing"
	"time"
)

func stopWeatherDamageTimer(st *FishState) {
	st.cancelBiteTimerLocked()
}
func fireWeatherDamageBite(s *Server, uid int64, st *FishState, ticket fishingTimerTicket) {
	st.mu.Lock()
	placeholder := time.NewTimer(time.Hour)
	if st.biteTimer == nil {
		st.biteTimer = placeholder
	}
	st.mu.Unlock()
	s.onBiteTimerTicket(uid, ticket)
	placeholder.Stop()
}
func TestWeatherBrokenHookDoesNotStopOtherHealthyHooks(t *testing.T) {
	s, db := newRigTestServer(t)
	ctx := t.Context()
	const uid = int64(1000001)
	rod, err := s.CreateItem(ctx, uid, "boatr_10222_SBH80_100", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !s.rigSetType(ctx, uid, rod.GUID, 59) {
		t.Fatal("marine rig rejected")
	}
	var hooks []string
	for i, id := range []string{"worm_01", "muckworm"} {
		hook, err := s.CreateItem(ctx, uid, "c_hook_20712", "backpack", 1)
		if err != nil {
			t.Fatal(err)
		}
		mounted := s.rigSetComponent(ctx, uid, rod.GUID, catHook, hook.GUID, int64(250+i), rigSetOpts{ParentType: familyRod, EnforceCategory: true})
		if mounted.Rejected || mounted.MountedGUID == "" {
			t.Fatal("hook mount rejected")
		}
		hooks = append(hooks, mounted.MountedGUID)
		baitTestMount(t, s, rod.GUID, id, 3, int64(350+i))
	}
	setWeatherTestTunable(t, s, "fish_weather_enabled", "1")
	setWeatherTestTunable(t, s, "bite_probability", "0")
	setWeatherTestTunable(t, s, "bite_delay_fixed", "60")
	addFishingRecoverySession(s, uid, 910)
	st := s.stateFor(uid)
	assertRetry := func(want bool) {
		t.Helper()
		st.mu.Lock()
		st.CastActive, st.InWater, st.Resolved, st.PushSent, st.Hooked = true, true, false, false, false
		st.CastTs, st.RigType = time.Now().Add(-time.Minute), 59
		st.GearGuid = rod.GUID
		ticket := fishingTimerTicket{st.castGeneration, st.biteTicket}
		st.mu.Unlock()
		fireWeatherDamageBite(s, uid, st, ticket)
		st.mu.Lock()
		retried := !st.Resolved && st.biteTimer != nil
		stopWeatherDamageTimer(st)
		st.mu.Unlock()
		if retried != want {
			t.Fatalf("retry=%v want=%v", retried, want)
		}
	}
	if err := db.SetItemDurability(ctx, hooks[0], 0); err != nil {
		t.Fatal(err)
	}
	if s.weatherRigBroken(ctx, uid, rod.GUID) {
		t.Fatal("one broken hook stopped a healthy baited hook")
	}
	assertRetry(true)
	// 天气鱼口会按概率丢弃单次判鱼，所以这里多试几次：
	// 真正"一根断钩让整根竿停口"会次次为空，偶发的天气空口不该算失败。
	rolled := false
	for i := 0; i < 20; i++ {
		if fish := s.rollFishForRig(ctx, uid, rod.GUID, "unknown-map", "worm_01", neutralFishingSkillBonus()); fish != nil {
			rolled = true
			break
		}
	}
	if !rolled {
		t.Fatal("one broken hook globally blocked actual roll")
	}
	if err := db.SetItemDurability(ctx, hooks[1], 0); err != nil {
		t.Fatal(err)
	}
	if !s.weatherRigBroken(ctx, uid, rod.GUID) {
		t.Fatal("all broken hooks kept fishing")
	}
	assertRetry(false)
	if fish := s.rollFishForRig(ctx, uid, rod.GUID, "unknown-map", "worm_01", neutralFishingSkillBonus()); fish != nil {
		t.Fatal("all broken hooks issued a fish")
	}
	if err := db.SetItemDurability(ctx, hooks[1], 1); err != nil {
		t.Fatal(err)
	}
	for _, slot := range []int64{gearCatReel, gearCatLine, gearCatLeader, gearCatLeaderWire} {
		item := &store.Item{GUID: NewItemGUID(), UID: uid, Container: "rig", RigGUID: rod.GUID, Qty: 1, FieldsJSON: "[]", Durability: 0, Workmanship: 1}
		if err := db.InsertItem(ctx, item); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertRigComponentAt(ctx, rod.GUID, slot, 0, item.GUID, 0); err != nil {
			t.Fatal(err)
		}
		if !s.weatherRigBroken(ctx, uid, rod.GUID) {
			t.Fatalf("broken common part %d did not stop fishing", slot)
		}
		assertRetry(false)
		if err := db.DeleteRigComponentsBySlot(ctx, rod.GUID, slot); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetItemDurability(ctx, rod.GUID, 0); err != nil {
		t.Fatal(err)
	}
	if !s.weatherRigBroken(ctx, uid, rod.GUID) {
		t.Fatal("broken shared rod did not stop fishing")
	}
	assertRetry(false)
}

func TestWeatherMissingHookPhysicsDoesNotBecomeKnownBreakage(t *testing.T) {
	s, db := newRigTestServer(t)
	rod := baitTestRod(t, s)
	const uid = int64(1000001)
	if err := db.UpsertRigComponentAt(t.Context(), rod, catHook, 0, NewItemGUID(), 250); err != nil {
		t.Fatal(err)
	}
	if s.weatherRigBroken(t.Context(), uid, rod) {
		t.Fatal("missing hook row became known breakage")
	}
	foreign := &store.Item{GUID: NewItemGUID(), UID: uid + 1, Container: "rig", RigGUID: rod, Qty: 1, FieldsJSON: "[]", Durability: 0, Workmanship: 1}
	if err := db.InsertItem(t.Context(), foreign); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRigComponentAt(t.Context(), rod, catHook, 1, foreign.GUID, 251); err != nil {
		t.Fatal(err)
	}
	if s.weatherRigBroken(t.Context(), uid, rod) {
		t.Fatal("another player's row became owned breakage")
	}
}

func TestWeatherRetryDoesNotResurrectInactiveCastOrReuseOldGeneration(t *testing.T) {
	s, _ := newRigTestServer(t)
	const uid = int64(1000001)
	setWeatherTestTunable(t, s, "fish_weather_enabled", "1")
	setWeatherTestTunable(t, s, "bite_delay_fixed", "60")
	addFishingRecoverySession(s, uid, 911)
	sess := s.liveSessionByUID(uid)
	st := s.stateFor(uid)
	st.mu.Lock()
	s.emptyWeatherBiteLocked(sess, st, time.Now())
	if !st.Resolved || st.biteTimer != nil || st.CastActive {
		st.mu.Unlock()
		t.Fatal("inactive cast was restarted")
	}
	st.CastActive, st.InWater, st.Hooked = true, true, false
	st.CastTs = time.Now().Add(-time.Minute)
	st.RigType = 12
	oldTicket := fishingTimerTicket{st.castGeneration, st.biteTicket}
	s.emptyWeatherBiteLocked(sess, st, time.Now())
	nextTicket := fishingTimerTicket{st.castGeneration, st.biteTicket}
	timer, due := st.biteTimer, st.BiteAt
	if timer == nil || nextTicket.generation != oldTicket.generation || nextTicket.sequence <= oldTicket.sequence {
		st.mu.Unlock()
		t.Fatal("retry reused stale timer generation")
	}
	st.mu.Unlock()
	s.onBiteTimerTicket(uid, oldTicket)
	st.mu.Lock()
	if st.biteTimer != timer || !st.BiteAt.Equal(due) || st.castGeneration != nextTicket.generation || st.biteTicket != nextTicket.sequence {
		stopWeatherDamageTimer(st)
		st.mu.Unlock()
		t.Fatal("stale callback replaced retry")
	}
	stopWeatherDamageTimer(st)
	st.mu.Unlock()
}

func TestWeatherBrokenExternalHookKeepsHealthyEmbeddedLure(t *testing.T) {
	s, db := newRigTestServer(t)
	ctx := t.Context()
	const uid = int64(1000001)
	rod, err := s.CreateItem(ctx, uid, "boatr_10222_SBH80_100", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !s.rigSetType(ctx, uid, rod.GUID, 55) {
		t.Fatal("dual-lure rig rejected")
	}
	external := mountHookForBaitTest(t, s, rod.GUID, 0)
	if err := db.SetItemDurability(ctx, external, 0); err != nil {
		t.Fatal(err)
	}
	_, lure := baitTestMount(t, s, rod.GUID, "spinnerbaits_1244", 1, 325)
	if s.weatherRigBroken(ctx, uid, rod.GUID) {
		t.Fatal("broken external hook masked the healthy embedded lure hook")
	}
	if err := db.SetItemDurability(ctx, lure, 0); err != nil {
		t.Fatal(err)
	}
	if !s.weatherRigBroken(ctx, uid, rod.GUID) {
		t.Fatal("broken lure should count as a broken known hook host")
	}
}
