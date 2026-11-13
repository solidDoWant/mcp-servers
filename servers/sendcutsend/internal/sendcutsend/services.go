package sendcutsend

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

// SendCutSend's website checks services in the browser; its API accepts
// whatever it is sent, including taps on holes that don't exist and
// unknown colours. So this file checks a services request against the
// part's geometry and the material's published rules before anything is
// priced or added to a cart.

const (
	// tapOversize is how much larger than the tap's drill a hole may be.
	// SendCutSend enlarges smaller holes to the drill size, but can't shrink
	// a hole that is too big for the thread.
	tapOversize = 0.003
	// minHangingHole is the smallest cutout, in inches, that coated parts
	// can hang from.
	minHangingHole = 0.063
)

// finishOperations are the services chosen as a single finish.
var finishOperations = []string{"powder", "anodizing", "plating"}

// ServicesInput is the services for one part, beyond cutting.
type ServicesInput struct {
	Bends     []BendInput `json:"bends,omitempty" jsonschema:"how to bend each bend line; when bending, every bend line from get_part_features must be listed"`
	Taps      []TapInput  `json:"taps,omitempty" jsonschema:"taps for holes, by hole ID from get_part_features"`
	Powder    string      `json:"powder,omitempty" jsonschema:"powder coat colour value from list_finishes, e.g. matte_black"`
	Anodizing string      `json:"anodizing,omitempty" jsonschema:"anodizing value from list_finishes (aluminium only)"`
	Plating   string      `json:"plating,omitempty" jsonschema:"plating value from list_finishes"`
	Deburring bool        `json:"deburring,omitempty" jsonschema:"deburr the part (SendCutSend adds it with powder coating)"`
	Tumbling  bool        `json:"tumbling,omitempty" jsonschema:"tumble the part"`
}

// BendInput configures one bend line.
type BendInput struct {
	ID        string  `json:"id" jsonschema:"bend line ID from get_part_features"`
	Angle     float64 `json:"angle" jsonschema:"bend angle in degrees, e.g. 90"`
	Direction string  `json:"direction" jsonschema:"up or down, seen from the drawing's front"`
}

// TapInput taps a set of holes with one tap.
type TapInput struct {
	HoleIDs []string `json:"hole_ids" jsonschema:"hole IDs from get_part_features"`
	Tap     string   `json:"tap" jsonschema:"tap size, e.g. M4 x 0.7 or 10-32"`
}

func (in *ServicesInput) empty() bool {
	return len(in.Bends) == 0 && len(in.Taps) == 0 && in.Powder == "" && in.Anodizing == "" &&
		in.Plating == "" && !in.Deburring && !in.Tumbling
}

// servicePlan is a checked services request, ready to send.
type servicePlan struct {
	operations []string
	counts     map[string]int
	bends      []BendConfig
	taps       []TapConfig
	finish     map[string]string
}

// quoteFields are the fields the quote endpoint prices services from: the
// services and how many bends and taps, not which ones.
func (p *servicePlan) quoteFields() map[string]any {
	if p == nil {
		return nil
	}

	out := map[string]any{"operations": p.operations, "operation_count": p.counts}
	for k, v := range p.finish {
		out[k+"_config"] = v
	}

	return out
}

// cartFields are the fields a cart line carries the services in.
func (p *servicePlan) cartFields() map[string]any {
	if p == nil {
		return nil
	}

	bends, taps := p.bends, p.taps
	if bends == nil {
		bends = []BendConfig{}
	}

	if taps == nil {
		taps = []TapConfig{}
	}

	out := map[string]any{
		"operations":  p.operations,
		"bends":       bends,
		"tap_config":  taps,
		"bend_config": map[string]any{"agreed_to_deformation": false},
	}

	for _, f := range finishOperations {
		out[f+"_config"] = p.finish[f]
	}

	return out
}

// partContext is what checking services needs to know about a part.
type partContext struct {
	drawing  *Drawing
	model    *Model
	material Material
	taps     map[string]TapSize
	finishes []Finish
}

func (s *session) partContext(ctx context.Context, d *Drawing, m Material, needTaps, needFinishes bool) (*partContext, error) {
	pc := &partContext{drawing: d, material: m}

	model, err := s.model(ctx, d)
	if err != nil {
		return nil, err
	}

	pc.model = model

	if needTaps {
		if pc.taps, err = s.tapSizes(ctx); err != nil {
			return nil, err
		}
	}

	if needFinishes {
		if pc.finishes, err = s.finishes(ctx); err != nil {
			return nil, err
		}
	}

	return pc, nil
}

func (pc *partContext) inches(v float64) float64 { return toInches(v, pc.drawing.Units) }

// plan checks a services request and returns what to send. Every problem
// found is reported at once.
func (pc *partContext) plan(in ServicesInput) (*servicePlan, error) {
	p := &servicePlan{counts: map[string]int{}, finish: map[string]string{}}

	var problems []string

	offers := func(op string) bool {
		if !slices.Contains(pc.material.Services(), op) {
			problems = append(problems, fmt.Sprintf("%s doesn't offer %s", pc.material.SKU, op))

			return false
		}

		return true
	}

	if len(in.Bends) > 0 && offers("bending") {
		problems = append(problems, pc.planBends(in.Bends, p)...)
	}

	if len(in.Taps) > 0 && offers("tapping") {
		problems = append(problems, pc.planTaps(in.Taps, len(in.Bends) > 0, p)...)
	}

	problems = append(problems, pc.planFinish(in, p, offers)...)

	if in.Deburring && offers("deburring") {
		p.operations = append(p.operations, "deburring")
	}

	if in.Tumbling && offers("tumbling") {
		p.operations = append(p.operations, "tumbling")
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("the services can't be used: %s", strings.Join(problems, "; "))
	}

	return p, nil
}

func (pc *partContext) planBends(in []BendInput, p *servicePlan) []string {
	var limits BendingLimits
	if err := pc.material.limits("bending", &limits); err != nil {
		return []string{err.Error()}
	}

	lines := map[string]*Entity{}
	for i := range pc.model.BendLines {
		lines[pc.model.BendLines[i].ID] = &pc.model.BendLines[i]
	}

	var problems []string

	seen := map[string]bool{}

	for _, b := range in {
		line, ok := lines[b.ID]

		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("part %d has no bend line %q (see get_part_features)", pc.drawing.ID, b.ID))

			continue
		case seen[b.ID]:
			problems = append(problems, fmt.Sprintf("bend line %s is listed twice", b.ID))

			continue
		}

		seen[b.ID] = true

		dir := strings.ToLower(strings.TrimSpace(b.Direction))
		if dir != "up" && dir != "down" {
			problems = append(problems, fmt.Sprintf("bend %s: direction must be up or down, not %q", b.ID, b.Direction))
		}

		if b.Angle < float64(limits.MinAngle) || b.Angle > float64(limits.MaxAngle) {
			problems = append(problems, fmt.Sprintf("bend %s: %s bends between %g° and %g°, not %g°",
				b.ID, pc.material.SKU, float64(limits.MinAngle), float64(limits.MaxAngle), b.Angle))
		}

		if l, ok := line.length(); ok && limits.MaxWidth > 0 && pc.inches(l) > float64(limits.MaxWidth) {
			problems = append(problems, fmt.Sprintf("bend %s is %.2f in long; %s bends at most %g in",
				b.ID, pc.inches(l), pc.material.SKU, float64(limits.MaxWidth)))
		}

		p.bends = append(p.bends, BendConfig{ID: b.ID, Angle: b.Angle, Direction: dir, Radius: float64(limits.BendRadius)})
	}

	var missing []string

	for id := range lines {
		if !seen[id] {
			missing = append(missing, id)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		problems = append(problems, fmt.Sprintf("every bend line needs a bend; missing %s", strings.Join(missing, ", ")))
	}

	p.operations = append(p.operations, "bending")
	p.counts["bending"] = len(p.bends)

	return problems
}

func (pc *partContext) planTaps(in []TapInput, bending bool, p *servicePlan) []string {
	var limits TappingLimits
	if err := pc.material.limits("tapping", &limits); err != nil {
		return []string{err.Error()}
	}

	allowed := map[string]bool{}
	for _, t := range limits.TapSizes {
		allowed[t.Tap] = true
	}

	holes := map[string]*Contour{}

	for i := range pc.model.Cut {
		if pc.model.Cut[i].Circular {
			holes[pc.model.Cut[i].ID] = &pc.model.Cut[i]
		}
	}

	var (
		problems []string
		seen     = map[string]bool{}
	)

	for _, t := range in {
		size, known := pc.taps[t.Tap]

		switch {
		case !allowed[t.Tap] || !known:
			problems = append(problems, fmt.Sprintf("%s can't be tapped %q; its taps are %s",
				pc.material.SKU, t.Tap, strings.Join(sortedKeys(allowed), ", ")))

			continue
		case len(t.HoleIDs) == 0:
			problems = append(problems, fmt.Sprintf("tap %s lists no holes", t.Tap))

			continue
		}

		for _, id := range t.HoleIDs {
			h, ok := holes[id]

			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("part %d has no round hole %q (see get_part_features)", pc.drawing.ID, id))

				continue
			case seen[id]:
				problems = append(problems, fmt.Sprintf("hole %s is tapped twice", id))

				continue
			}

			seen[id] = true

			if msg := pc.tapProblem(h, size, float64(limits.MinHoleToEdge), bending); msg != "" {
				problems = append(problems, msg)
			}

			p.taps = append(p.taps, TapConfig{ID: id, Tap: t.Tap})
		}
	}

	p.operations = append(p.operations, "tapping")
	p.counts["tapping"] = len(p.taps)

	return problems
}

// tapProblem applies SendCutSend's tapping rules to one hole: no larger
// than the tap's drill, and its centre at least half the drill plus the
// material's minimum from any other edge, and half the bending die from a
// bend line.
func (pc *partContext) tapProblem(h *Contour, size TapSize, minToEdge float64, bending bool) string {
	center, dia, ok := h.hole()
	if !ok {
		return fmt.Sprintf("hole %s isn't a round hole", h.ID)
	}

	diaIn := pc.inches(dia)
	if diaIn > size.Drill+tapOversize {
		return fmt.Sprintf("hole %s is %.4f in across, larger than the %.4f in hole a %s tap needs; draw it at the tap's hole size",
			h.ID, diaIn, size.Drill, size.Tap)
	}

	clear, measured := pc.model.clearance(center, h.ID)
	need := size.Drill/2 + minToEdge

	if measured && pc.inches(clear) < need {
		return fmt.Sprintf("hole %s's centre is %.3f in from the nearest edge; a %s tap needs %.3f in",
			h.ID, pc.inches(clear), size.Tap, need)
	}

	if bending {
		var limits BendingLimits
		if pc.material.limits("bending", &limits) == nil && limits.BottomDie > 0 {
			for i := range pc.model.BendLines {
				d, ok := pc.model.BendLines[i].distanceTo(center)
				if ok && pc.inches(d) < float64(limits.BottomDie)/2 {
					return fmt.Sprintf("hole %s is %.3f in from bend line %s; tapped holes must be at least %.3f in from bends",
						h.ID, pc.inches(d), pc.model.BendLines[i].ID, float64(limits.BottomDie)/2)
				}
			}
		}
	}

	return ""
}

func (pc *partContext) planFinish(in ServicesInput, p *servicePlan, offers func(string) bool) []string {
	chosen := map[string]string{"powder": in.Powder, "anodizing": in.Anodizing, "plating": in.Plating}

	var picked []string

	for _, f := range finishOperations {
		if strings.TrimSpace(chosen[f]) != "" {
			picked = append(picked, f)
		}
	}

	switch len(picked) {
	case 0:
		return nil
	case 1:
	default:
		return []string{fmt.Sprintf("choose one finish, not %s", strings.Join(picked, " and "))}
	}

	op := picked[0]
	value := strings.ToLower(strings.TrimSpace(chosen[op]))

	var problems []string

	if !offers(op) {
		return nil
	}

	var options []string

	found := false

	for _, f := range pc.finishes {
		if f.FinishType == op {
			options = append(options, f.Value)
			found = found || f.Value == value
		}
	}

	if !found {
		problems = append(problems, fmt.Sprintf("%q isn't a %s option; choose one of %s", chosen[op], op, strings.Join(options, ", ")))
	}

	if !pc.hasHangingHole() {
		problems = append(problems, fmt.Sprintf("%s needs a hole or cutout at least %.3f in across to hang the part from", op, minHangingHole))
	}

	p.operations = append(p.operations, op)
	p.finish[op] = value

	return problems
}

// hasHangingHole reports whether the part has a cutout coated parts can
// hang from.
func (pc *partContext) hasHangingHole() bool {
	outer := pc.model.outer()

	for i := range pc.model.Cut {
		if i == outer {
			continue
		}

		b, ok := pc.model.Cut[i].bounds()
		if !ok {
			continue
		}

		if pc.inches(math.Min(b.Top-b.Bottom, b.Right-b.Left)) >= minHangingHole {
			return true
		}
	}

	return false
}

// tapsThatFit lists the material's taps whose hole this one can be cut to
// and that pass the edge and bend rules.
func (pc *partContext) tapsThatFit(h *Contour) []string {
	var limits TappingLimits
	if pc.material.limits("tapping", &limits) != nil {
		return nil
	}

	var fit []string

	for _, t := range limits.TapSizes {
		size, ok := pc.taps[t.Tap]
		if !ok {
			continue
		}

		_, dia, _ := h.hole()
		// Holes much smaller than the drill are probably meant for
		// something else, so only suggest taps close to the hole's size.
		if pc.inches(dia) >= size.Drill*0.85 && pc.tapProblem(h, size, float64(limits.MinHoleToEdge), len(pc.model.BendLines) > 0) == "" {
			fit = append(fit, t.Tap)
		}
	}

	return fit
}

// savedServices turns a part's saved configuration into a services request.
func savedServices(sp *SavedPart) *ServicesInput {
	if sp == nil {
		return nil
	}

	in := &ServicesInput{
		Powder: sp.Powder, Anodizing: sp.Anodizing, Plating: sp.Plating,
		Deburring: slices.Contains(sp.Operations, "deburring"),
		Tumbling:  slices.Contains(sp.Operations, "tumbling"),
	}

	for _, b := range sp.Bending {
		in.Bends = append(in.Bends, BendInput{ID: b.ID, Angle: b.Angle, Direction: b.Direction})
	}

	byTap := map[string][]string{}

	var order []string

	for _, t := range sp.Tapping {
		if _, ok := byTap[t.Tap]; !ok {
			order = append(order, t.Tap)
		}

		byTap[t.Tap] = append(byTap[t.Tap], t.ID)
	}

	for _, tap := range order {
		in.Taps = append(in.Taps, TapInput{Tap: tap, HoleIDs: byTap[tap]})
	}

	if in.empty() {
		return nil
	}

	return in
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

// errNeedsMaterial is returned when services are asked for without a
// material, which decides what is possible.
var errNeedsMaterial = errors.New("services need a material_sku: what can be done depends on the material")
