package automix

import (
	"fmt"
	"math"
	"strings"
)

// TransitionTrack 规划器看到的一首歌。
type TransitionTrack struct {
	// Duration 秒。非有限值（直播流等）无从排程。
	Duration float64
	// LastSung 歌词时间轴上最后一句的结束时刻（仅出场曲使用）。nil 表示无歌词或纯音乐。
	LastSung *float64
	// HasLyrics 是否拿到了歌词（仅用于日志区分"无歌词"与"歌词里没唱"）。
	HasLyrics bool
	Profile   *TrackProfile
	// VocalEnd 由分离出的人声测得的最后演唱时刻（曲中秒数，仅出场曲）。nil 表示没测到：
	// 未分离、窗口还没好，或曲尾窗口里根本没唱。有它时优先于歌词——歌词的结束时刻常是
	// 最后一句起点加五秒的估计，不是测量。
	VocalEnd *float64
	// Separated 四轨手法为这一端持有的分离窗口（曲中秒数），nil 表示没有。
	// 一个窗口存在即能托住人声，它的范围就是能托多长的过渡——同一件事，所以是同一个字段。
	Separated *Extent
}

// Extent 曲中的一段 [From, To)，秒。
type Extent struct{ From, To float64 }

// TransitionKind 硬切或过渡。
type TransitionKind string

const (
	KindHardCut TransitionKind = "hardCut"
	KindFade    TransitionKind = "fade"
)

// TransitionPlan 规划层与执行层之间唯一的契约。
type TransitionPlan struct {
	Kind     TransitionKind
	Style    TransitionStyle
	Relation KeyRelation
	Tempo    TempoRelation
	// Stretch 出场曲的播放速率（本移植暂不执行变速，见执行层）。
	Stretch   float64
	TiltDB    [2]float64
	EchoThrow bool
	// OutStart 出场曲第几秒开始过渡。
	OutStart float64
	// InStart 进场曲从第几秒起播。
	InStart float64
	// Overlap 本次过渡占用出场曲尾部多少秒。
	Overlap    float64
	MinOverlap float64
	Reason     string
}

const (
	// MaxOverlapSec 过渡长度的天花板。
	MaxOverlapSec = 25.0
	// MinOverlapSec 低于此不再是过渡。
	MinOverlapSec       = 0.8
	minCutRoomSec       = 0.15
	defaultOverlapSec   = 5.0
	defaultOverlapBeats = BeatsPerPhrase * 2
	gridSnapBeats       = 1
	sectionSnapSec      = 2.0
	maxTrimmedTailSec   = 10.0
	// EntryMarginSec 保留一点进场曲的前导静音，避免切进第一个瞬态。
	EntryMarginSec = 0.1
)

func beatSecOf(bpm *float64) *float64 {
	if v, ok := positive(bpm); ok && !math.IsInf(v, 0) {
		return ptr(60 / v)
	}
	return nil
}

// EndsOf 出场曲的两个"结束"：Sounding 之后是数字静音，Body 之后是衰减。
func EndsOf(t TransitionTrack) (sounding, body float64) {
	var p *TrackProfile
	if t.Profile != nil && !t.Profile.Partial {
		p = t.Profile
	}
	back := func(seconds *float64) float64 {
		if seconds != nil && *seconds > 0 {
			return t.Duration - math.Min(*seconds, maxTrimmedTailSec)
		}
		return t.Duration
	}
	if p == nil {
		return t.Duration, t.Duration
	}
	return back(p.LeadOut), back(p.BodyOut)
}

func barPhase(g Grid, seconds float64) float64 {
	return modulo(g.Offset-seconds, g.Period)
}

func nearestWithin(moments []float64, target, tolerance float64, usable func(float64) bool) *float64 {
	var best *float64
	for _, m := range moments {
		if math.Abs(m-target) > tolerance || !usable(m) {
			continue
		}
		if best == nil || math.Abs(m-target) < math.Abs(*best-target) {
			best = ptr(m)
		}
	}
	return best
}

// HardCut 结尾本身不可知时的硬切。
func HardCut(reason string) TransitionPlan {
	return TransitionPlan{
		Kind: KindHardCut, Style: StylePlainBlend, Relation: KeyUnknown, Tempo: TempoUnknown,
		Stretch: 1, MinOverlap: MinOverlapSec, Reason: reason,
	}
}

func fmtS(v float64) string { return fmt.Sprintf("%.2fs", round2(v)) }

// PlanTransition 决定两首歌之间用哪种接法、占用出场曲多少。纯函数。
//
// bpm 出场曲的速度（设定长度单位）；at 出场曲当前位置，此前的部分已不是余量。
func PlanTransition(from, to TransitionTrack, bpm *float64, at float64) TransitionPlan {
	if math.IsInf(from.Duration, 0) || math.IsNaN(from.Duration) || from.Duration <= 0 {
		return HardCut("outgoing duration unknown, nothing to schedule a fade against")
	}
	choice := ChooseTransitionStyle(from.Profile, to.Profile)
	end, body := EndsOf(from)
	left := math.Max(0, end-at)
	trimmed := from.Duration - end
	silence := ""
	if trimmed > 0.05 {
		silence = ", skipped " + fmtS(trimmed) + " of silence at the end"
	}
	clipped := ""
	if from.Profile != nil && from.Profile.BodyOut != nil && *from.Profile.BodyOut > maxTrimmedTailSec {
		clipped = fmt.Sprintf(", its last %s is decay but only %gs of that may be trimmed", fmtS(*from.Profile.BodyOut), maxTrimmedTailSec)
	}

	if choice.Style == StyleBeatCut {
		placeable := CutLeadSec
		if to.Profile != nil {
			placeable = to.Profile.LeadIn + HeadBudgetSec
		}
		room := math.Min(math.Min(CutLeadSec, end/4), math.Min(placeable, left))
		if room < minCutRoomSec {
			if room == left {
				return HardCut("only " + fmtS(left) + " of the outgoing track left to cut in")
			}
			return HardCut("track too short to place a cut in (" + fmtS(end) + ")")
		}
		return TransitionPlan{
			Kind: KindFade, Style: choice.Style, Relation: choice.Relation, Tempo: choice.Tempo.Relation,
			Stretch: 1, EchoThrow: choice.EchoThrow,
			OutStart: round2(end - room), InStart: 0, Overlap: round2(room), MinOverlap: minCutRoomSec,
			Reason: fmt.Sprintf("%s - %s%s", choice.Style, choice.Reason, silence),
		}
	}

	beat := beatSecOf(bpm)
	// 有测量时直接用测量，而不是取两者较晚的：歌词结束时刻是个五秒常数，
	// 取 max 会让一个编出来的数大约一半时候把测得的下限推后。猜测对测量没有投票权。
	lastSung, sungFrom := from.LastSung, "the lyric file"
	sungGap := ""
	if from.VocalEnd != nil {
		lastSung, sungFrom = from.VocalEnd, "the vocal stem"
		if from.LastSung != nil && math.Abs(*from.VocalEnd-*from.LastSung) > 0.5 {
			sungGap = ", the lyric file said " + fmtS(*from.LastSung)
		}
	}
	var tail *float64
	if lastSung != nil {
		tail = ptr(math.Max(0, end-*lastSung))
	}
	var intro *float64
	if to.Profile != nil {
		intro = to.Profile.SectionStart
	}
	vouchFor := func(w *float64) *float64 {
		if w != nil && *w >= MinOverlapSec {
			return w
		}
		return nil
	}
	tailRoom, introRoom := vouchFor(tail), vouchFor(intro)
	// 只要一端能担保整段过渡里没有它自己的人声即可——是 max 不是 min。
	singleVoiceRoom := math.Inf(1)
	if tailRoom != nil || introRoom != nil {
		singleVoiceRoom = 0
		if tailRoom != nil {
			singleVoiceRoom = *tailRoom
		}
		if introRoom != nil {
			singleVoiceRoom = math.Max(singleVoiceRoom, *introRoom)
		}
	}
	toLeadIn := 0.0
	if to.Profile != nil {
		toLeadIn = to.Profile.LeadIn
	}
	entryFloor := math.Min(maxTrimmedTailSec, math.Max(0, toLeadIn-EntryMarginSec))
	// 两个分离窗口实际能托住的最长过渡。手法拒绝任何两端没被窗口盖住的过渡、退回主交叉淡化，
	// 而那是唯一托不住人声的执行器。后端：过渡最晚在 end 结束，整段须在 end 与首个分离采样之间；
	// 前端：进场窗口还要盖住起播点（最多 maxTrimmedTailSec）。留一拍给对齐。
	gestureRoom := math.Inf(1)
	if from.Separated != nil && to.Separated != nil {
		slack := 0.0
		if beat != nil {
			slack = *beat
		}
		gestureRoom = math.Max(0, math.Min(end-from.Separated.From, to.Separated.To-entryFloor-slack))
	}

	wanted := defaultOverlapSec
	if beat != nil {
		wanted = *beat * defaultOverlapBeats
	}
	wanted *= choice.LengthScale
	ceiling := math.Min(math.Min(math.Min(MaxOverlapSec, end/4), math.Min(singleVoiceRoom, left)), gestureRoom)
	overlap := QuantiseToMusic(math.Min(wanted, ceiling), beat, ceiling)

	if overlap < MinOverlapSec {
		var bound string
		switch {
		case ceiling == left:
			bound = "only " + fmtS(left) + " of the outgoing track left"
		case !math.IsInf(singleVoiceRoom, 0) && singleVoiceRoom <= end/4:
			if tailRoom != nil && singleVoiceRoom == *tailRoom {
				bound = "only " + fmtS(singleVoiceRoom) + " after the outgoing track stops singing"
			} else {
				bound = "only " + fmtS(singleVoiceRoom) + " before the next track sings"
			}
		default:
			bound = fmt.Sprintf("a %s track leaves only %s to fade across", fmtS(end), fmtS(end/4))
		}
		return HardCut("no room to fade - " + bound)
	}

	outroMissing := "no lyrics for the outgoing track"
	if from.HasLyrics {
		outroMissing = "nothing sung in the outgoing lyrics"
	}
	introMissing := "the incoming track was never analysed"
	if to.Profile != nil {
		introMissing = "nothing measurable at the start of the incoming track"
	}
	window := outroMissing
	if tail != nil {
		if intro == nil {
			window = introMissing
		} else {
			window = fmt.Sprintf("outro %s, intro %s", fmtS(*tail), fmtS(*intro))
		}
	}
	length := "default"
	if beat != nil {
		length = fmt.Sprintf("%g beats", round2(overlap / *beat))
	}
	if overlap < wanted-0.05 {
		switch {
		case ceiling < wanted-0.05 && ceiling == left:
			length += fmt.Sprintf(", wanted %s, capped by the %s left of this track", fmtS(wanted), fmtS(left))
		case ceiling < wanted-0.05:
			length += fmt.Sprintf(", wanted %s, capped by what the pair has room for (%s)", fmtS(wanted), fmtS(ceiling))
		default:
			length += fmt.Sprintf(", wanted %s, rounded down to the grid", fmtS(wanted))
		}
	}

	latest := end - overlap
	floorAt := math.Max(body, at)
	if lastSung != nil {
		floorAt = math.Max(floorAt, *lastSung)
	}
	anchor := math.Min(latest, floorAt)
	fadeOut := ""
	if rode := latest - anchor; rode > 0.05 {
		fadeOut = ", started " + fmtS(rode) + " early to ride the fade-out"
	}

	lowest := math.Min(anchor, latest)
	withinRange := func(v float64) bool { return v >= lowest-1e-6 && v <= latest+1e-6 }
	var sections []float64
	var fromBeatOffset *float64
	if from.Profile != nil {
		sections = from.Profile.Sections
		fromBeatOffset = &from.Profile.BeatOffset
	}
	boundary := nearestWithin(sections, anchor, sectionSnapSec, withinRange)
	grid := BarGrid(bpm, fromBeatOffset, 1)
	base := anchor
	if boundary != nil {
		base = *boundary
	}
	tol := 0.0
	if beat != nil {
		tol = *beat * gridSnapBeats
	}
	snapped := SnapToGrid(base, grid, tol)
	outStart := base
	if withinRange(snapped) {
		outStart = snapped
	}
	placed := ""
	if boundary != nil {
		placed = ", on a section edge"
	} else if math.Abs(outStart-anchor) > 0.01 {
		placed = ", on a beat"
	}

	var toGrid *Grid
	if to.Profile != nil {
		toGrid = BarGrid(to.Profile.BPM, &to.Profile.BeatOffset, 1)
	}
	var phase *float64
	if grid != nil {
		phase = ptr(barPhase(*grid, outStart) / choice.Tempo.Stretch)
	}
	inStart := AlignEntry(entryFloor, toGrid, phase)

	var extra strings.Builder
	if choice.Relation != KeyUnknown {
		extra.WriteString(", " + string(choice.Relation) + " keys")
	}
	switch {
	case from.Profile == nil:
		extra.WriteString(", outgoing never analysed")
	case from.Profile.Partial:
		extra.WriteString(", outgoing tail not analysed")
	}
	if lastSung == nil {
		extra.WriteString(", nothing says where the singing stops")
	} else {
		extra.WriteString(", sung to " + fmtS(*lastSung) + " off " + sungFrom + sungGap)
	}
	extra.WriteString(silence + clipped + fadeOut + placed)
	apart := int(math.Round(math.Abs(choice.Tempo.Ratio-1) * 100))
	switch {
	case choice.Tempo.Stretch != 1:
		extra.WriteString(fmt.Sprintf(", outgoing wants a %.1f%% bend onto the next tempo", (choice.Tempo.Stretch-1)*100))
	case choice.Tempo.Relation == TempoDrifting:
		extra.WriteString(fmt.Sprintf(", tempos %d%% apart, left to drift", apart))
	case choice.Tempo.Relation == TempoFar:
		extra.WriteString(fmt.Sprintf(", tempos %d%% apart, too far to overlap", apart))
	}
	if inStart > 0.05 {
		extra.WriteString(", entering the next track at " + fmtS(inStart))
	}
	nextSings := intro != nil && inStart+overlap > *intro
	outgoingSings := lastSung != nil && outStart < *lastSung
	switch {
	case nextSings && outgoingSings:
		extra.WriteString(", BOTH tracks sing inside this blend")
	case nextSings:
		extra.WriteString(", the next track sings over the outgoing instrumental")
	case outgoingSings:
		extra.WriteString(", the outgoing track sings over the next one's intro")
	}

	return TransitionPlan{
		Kind: KindFade, Style: choice.Style, Relation: choice.Relation, Tempo: choice.Tempo.Relation,
		Stretch: choice.Tempo.Stretch, TiltDB: choice.TiltDB, EchoThrow: choice.EchoThrow,
		OutStart: round2(outStart), InStart: round2(inStart), Overlap: round2(overlap), MinOverlap: MinOverlapSec,
		Reason: fmt.Sprintf("%s %s %s - %s (%s%s)", choice.Style, fmtS(overlap), length, choice.Reason, window, extra.String()),
	}
}

// ResolveOverlap 进场曲真正起播时的最后一道闸：按实际剩余量收紧，不够就放弃过渡。
func ResolveOverlap(plan TransitionPlan, remaining float64) float64 {
	if plan.Kind != KindFade || math.IsInf(remaining, 0) || math.IsNaN(remaining) {
		return 0
	}
	overlap := math.Min(plan.Overlap, remaining)
	if overlap >= plan.MinOverlap {
		return round2(overlap)
	}
	return 0
}

// 交叉淡化模式的参数范围。
const (
	CrossfadeMinSec     = 1
	CrossfadeMaxSec     = MaxOverlapSec
	CrossfadeDefaultSec = 5
)

// ClampCrossfadeSeconds 用户设定的交叉淡化秒数。
func ClampCrossfadeSeconds(seconds float64) float64 {
	if math.IsInf(seconds, 0) || math.IsNaN(seconds) || seconds <= 0 {
		return CrossfadeDefaultSec
	}
	return math.Min(CrossfadeMaxSec, math.Max(CrossfadeMinSec, math.Round(seconds)))
}

// PlanCrossfade 可预测的那一半：只看两个时长与用户设定的秒数（加上两端数字静音）。
func PlanCrossfade(from, to TransitionTrack, seconds, at float64) TransitionPlan {
	if math.IsInf(from.Duration, 0) || math.IsNaN(from.Duration) || from.Duration <= 0 {
		return HardCut("outgoing duration unknown, nothing to schedule a fade against")
	}
	end, _ := EndsOf(from)
	leadIn := 0.0
	if to.Profile != nil {
		leadIn = to.Profile.LeadIn
	}
	inStart := math.Max(0, leadIn-EntryMarginSec)
	asked := ClampCrossfadeSeconds(seconds)
	incoming := math.Inf(1)
	if to.Duration > 0 && !math.IsInf(to.Duration, 0) {
		incoming = math.Max(0, to.Duration-inStart)
	}
	room := math.Min(math.Min(math.Max(0, end-at), incoming), end/4)
	overlap := math.Min(asked, room)
	if overlap < MinOverlapSec {
		return HardCut("no room to crossfade - " + fmtS(math.Max(0, room)) + " available")
	}
	reason := "crossfade " + fmtS(overlap)
	if overlap < asked-0.05 {
		reason += fmt.Sprintf(", capped from %gs by %s of room", asked, fmtS(room))
	}
	if inStart > 0.05 {
		reason += ", entering the next track at " + fmtS(inStart)
	}
	return TransitionPlan{
		Kind: KindFade, Style: StylePlainBlend, Relation: KeyUnknown, Tempo: TempoUnknown, Stretch: 1,
		OutStart: round2(end - overlap), InStart: round2(inStart), Overlap: round2(overlap),
		MinOverlap: MinOverlapSec, Reason: reason,
	}
}

// Mode 过渡模式。
type Mode string

const (
	ModeOff       Mode = "off"
	ModeCrossfade Mode = "crossfade"
	ModeAutomix   Mode = "automix"
)

// Settings 过渡设置。
type Settings struct {
	Mode             Mode
	CrossfadeSeconds float64
}

// PlanForMode 按模式挑规划器。automix 没有任何证据时退回默认交叉淡化。
func PlanForMode(s Settings, from, to TransitionTrack, bpm *float64, at float64) TransitionPlan {
	if s.Mode == ModeCrossfade {
		return PlanCrossfade(from, to, s.CrossfadeSeconds, at)
	}
	_, hasBPM := positive(bpm)
	if from.Profile != nil || to.Profile != nil || hasBPM || from.HasLyrics {
		return PlanTransition(from, to, bpm, at)
	}
	plan := PlanCrossfade(from, to, CrossfadeDefaultSec, at)
	plan.Reason = "automix has no evidence here, " + plan.Reason + " instead"
	return plan
}
