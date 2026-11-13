package sendcutsend

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/mcp-servers/internal/mcptest"
)

// The bracket fixture is a real part: 3 bend lines, an M4 hole
// (eec946baa4e, 0.1343 in) and two 5 mm holes, drawn in millimetres. Its
// saved configuration bends all three lines, taps the M4 hole and powder
// coats it matte black.

func bracketServices() map[string]any {
	return map[string]any{
		"bends": []map[string]any{
			{"id": "e64ebc60c64", "angle": 90, "direction": "up"},
			{"id": "ef470022851", "angle": 90, "direction": "down"},
			{"id": "e61229e1e29", "angle": 90, "direction": "up"},
		},
		"taps":   []map[string]any{{"hole_ids": []string{"eec946baa4e"}, "tap": "M4 x 0.7"}},
		"powder": "matte_black",
	}
}

func TestGetPartFeatures(t *testing.T) {
	t.Run("lists bend lines and holes in inches with the taps that fit and the saved services", func(t *testing.T) {
		f := newFake(t)

		var got FeaturesOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "get_part_features", map[string]any{
			"part_id": bracketID, "material_sku": "CRS-074",
		}), &got)

		assert.Equal(t, "mm", got.Units)
		require.Len(t, got.BendLines, 3)
		assert.InDelta(t, 3.1102, got.BendLines[2].LengthIn, 0.0001, "79 mm")

		require.Len(t, got.Holes, 3)
		assert.Equal(t, HoleInfo{
			ID: "eec946baa4e", DiameterIn: 0.1343, CenterX: 0.178, CenterY: 1.762,
			TapsThatFit: []string{"M4 x 0.7", "8-32"},
		}, got.Holes[0], "smallest first, with taps whose hole it can be cut to")
		assert.Equal(t, []string{"M6 x 1.0"}, got.Holes[1].TapsThatFit)

		assert.Equal(t, &BendLimits{MinAngle: 5, MaxAngle: 130, MaxBendLengthIn: 44, MinFlangeLengthIn: 0.255}, got.BendLimits)

		var want ServicesInput

		raw, _ := json.Marshal(bracketServices())
		require.NoError(t, json.Unmarshal(raw, &want))
		assert.Equal(t, &want, got.SavedServices)
	})

	t.Run("leaves out taps and limits without a material", func(t *testing.T) {
		f := newFake(t)

		var got FeaturesOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "get_part_features", map[string]any{"part_id": bracketID}), &got)

		assert.Nil(t, got.BendLimits)
		assert.Empty(t, got.Holes[0].TapsThatFit)
		assert.Zero(t, f.callCount("GET /tap_sizes"))
	})

	t.Run("refuses geometry hosted anywhere but SendCutSend's CDN", func(t *testing.T) {
		f := newFake(t)
		f.setOverride("GET /drawings/{id}", f.signed(func(w http.ResponseWriter, _ *http.Request) {
			f.write(w, strings.ReplaceAll(f.fixture("drawing-bracket.json"), f.srv.URL+"/cdn/", "https://evil.example/"))
		}))

		msg := errorText(t, connect(t, f.options(t)), "get_part_features", map[string]any{"part_id": bracketID})

		assert.Contains(t, msg, "unexpected address")
	})
}

func TestQuoteWithServices(t *testing.T) {
	t.Run("prices the services and sends only what the quote needs", func(t *testing.T) {
		f := newFake(t)

		var got QuoteOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "quote_part", map[string]any{
			"part_id": bracketID, "quantity": 2, "material_sku": "CRS-074", "services": bracketServices(),
		}), &got)

		require.Len(t, got.Quotes, 1)
		q := got.Quotes[0]
		assert.InDelta(t, 12.64, q.TotalPrice, 0.001, "cutting")
		assert.Equal(t, []ServiceCost{
			{Service: "bending", Count: 3, TotalUSD: 16.08},
			{Service: "powder", TotalUSD: 14.88},
			{Service: "tapping", Count: 1, TotalUSD: 6.86},
		}, q.Services)
		require.NotNil(t, q.GrandTotal)
		assert.InDelta(t, 50.46, *q.GrandTotal, 0.001)

		body := f.lastBody("POST /drawings/6001/quotes")
		assert.Equal(t, []any{"bending", "tapping", "powder"}, body["operations"])
		assert.Equal(t, map[string]any{"bending": 3.0, "tapping": 1.0}, body["operation_count"])
		assert.Equal(t, "matte_black", body["powder_config"])
		assert.NotContains(t, body, "bends", "the quote prices bends by count")
	})

	t.Run("needs a material before calling SendCutSend", func(t *testing.T) {
		f := newFake(t)

		msg := errorText(t, connect(t, f.options(t)), "quote_part", map[string]any{
			"part_id": bracketID, "quantity": 2, "services": bracketServices(),
		})

		assert.Contains(t, msg, "need a material_sku")
		assert.Empty(t, f.callList())
	})

	rejected := []struct {
		name     string
		sku      string
		services func(map[string]any)
		want     []string
	}{
		{"a bend line the part doesn't have", "CRS-074", func(s map[string]any) {
			s["bends"] = append(s["bends"].([]map[string]any), map[string]any{"id": "nope", "angle": 90, "direction": "up"})
		}, []string{`no bend line "nope"`}},
		{"bend lines left out", "CRS-074", func(s map[string]any) {
			s["bends"] = s["bends"].([]map[string]any)[:1]
		}, []string{"every bend line needs a bend; missing e61229e1e29, ef470022851"}},
		{"an angle beyond the material's limit", "CRS-074", func(s map[string]any) {
			s["bends"].([]map[string]any)[0]["angle"] = 150
		}, []string{"CRS-074 bends between 5° and 130°, not 150°"}},
		{"a direction other than up or down", "CRS-074", func(s map[string]any) {
			s["bends"].([]map[string]any)[0]["direction"] = "sideways"
		}, []string{`direction must be up or down, not "sideways"`}},
		{"a hole the part doesn't have", "CRS-074", func(s map[string]any) {
			s["taps"] = []map[string]any{{"hole_ids": []string{"nope"}, "tap": "M4 x 0.7"}}
		}, []string{`no round hole "nope"`}},
		{"a hole too big for its tap", "CRS-074", func(s map[string]any) {
			s["taps"] = []map[string]any{{"hole_ids": []string{"e12cd1bee5f"}, "tap": "M4 x 0.7"}}
		}, []string{"hole e12cd1bee5f is 0.1969 in across, larger than the 0.1340 in hole a M4 x 0.7 tap needs"}},
		{"a tap the material doesn't offer", "CRS-074", func(s map[string]any) {
			s["taps"] = []map[string]any{{"hole_ids": []string{"eec946baa4e"}, "tap": "1/4-20"}}
		}, []string{`CRS-074 can't be tapped "1/4-20"`}},
		{"a hole tapped twice", "CRS-074", func(s map[string]any) {
			s["taps"] = []map[string]any{
				{"hole_ids": []string{"eec946baa4e"}, "tap": "M4 x 0.7"},
				{"hole_ids": []string{"eec946baa4e"}, "tap": "8-32"},
			}
		}, []string{"hole eec946baa4e is tapped twice"}},
		{"two finishes", "CRS-074", func(s map[string]any) {
			s["plating"] = "clear_zinc"
		}, []string{"choose one finish, not powder and plating"}},
		{"a colour that doesn't exist", "CRS-074", func(s map[string]any) {
			s["powder"] = "hot_pink"
		}, []string{`"hot_pink" isn't a powder option`}},
		{"a service the material doesn't offer", "ABSBLACK-125", func(map[string]any) {}, []string{"ABSBLACK-125 doesn't offer bending", "ABSBLACK-125 doesn't offer powder"}},
	}

	for _, tc := range rejected {
		t.Run("rejects "+tc.name+" without asking for a price", func(t *testing.T) {
			f := newFake(t)
			services := bracketServices()
			tc.services(services)

			msg := errorText(t, connect(t, f.options(t)), "quote_part", map[string]any{
				"part_id": bracketID, "quantity": 2, "material_sku": tc.sku, "services": services,
			})

			for _, w := range tc.want {
				assert.Contains(t, msg, w)
			}

			assert.Zero(t, f.callCount("POST /drawings/6001/quotes"))
		})
	}
}

func TestCreateSavedCartWithServices(t *testing.T) {
	t.Run("sends every bend with the material's radius and every tapped hole to the cart", func(t *testing.T) {
		f := newFake(t)

		var got CartOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "create_saved_cart", map[string]any{
			"title": "Brackets",
			"items": []map[string]any{{"part_id": bracketID, "material_sku": "CRS-074", "quantity": 2, "services": bracketServices()}},
		}), &got)

		require.Len(t, got.Items, 1)
		assert.InDelta(t, 50.46, *got.Items[0].TotalPrice, 0.001, "the line's total includes its services")
		assert.Equal(t, []ServiceCost{
			{Service: "bending", Count: 3, TotalUSD: 16.08},
			{Service: "deburring", TotalUSD: 0},
			{Service: "powder", TotalUSD: 14.88},
			{Service: "tapping", Count: 1, TotalUSD: 6.86},
		}, got.Items[0].Services, "SendCutSend adds free deburring with powder coating")

		body := f.lastBody("POST /cart")
		assert.InDelta(t, 45, body["material_id"], 0)
		assert.Equal(t, []any{"bending", "tapping", "powder"}, body["operations"])
		assert.Equal(t, []any{
			map[string]any{"id": "e64ebc60c64", "angle": 90.0, "direction": "up", "radius": 0.063},
			map[string]any{"id": "ef470022851", "angle": 90.0, "direction": "down", "radius": 0.063},
			map[string]any{"id": "e61229e1e29", "angle": 90.0, "direction": "up", "radius": 0.063},
		}, body["bends"])
		assert.Equal(t, []any{map[string]any{"id": "eec946baa4e", "tap": "M4 x 0.7"}}, body["tap_config"])
		assert.Equal(t, "matte_black", body["powder_config"])
		assert.Empty(t, body["anodizing_config"])
		assert.Equal(t, map[string]any{"agreed_to_deformation": false}, body["bend_config"])
		assert.Zero(t, f.cartLen())
	})

	t.Run("rejects bad services before touching the cart", func(t *testing.T) {
		f := newFake(t)
		services := bracketServices()
		services["powder"] = "hot_pink"

		msg := errorText(t, connect(t, f.options(t)), "create_saved_cart", map[string]any{
			"title": "Brackets",
			"items": []map[string]any{{"part_id": bracketID, "material_sku": "CRS-074", "quantity": 2, "services": services}},
		})

		assert.Contains(t, msg, "items[0]: the services can't be used")
		assert.Zero(t, f.callCount("GET /cart"))
	})
}

// A 1 by 1 inch square plate, in inches, with whatever holes and bend lines
// a test adds.
func squarePlate() *Model {
	sq := []Point{{0, 0}, {1, 0}, {1, 1}, {0, 1}}

	outer := Contour{ID: "outer", Closed: true, Area: 1}

	for i := range sq {
		a, b := sq[i], sq[(i+1)%4]
		outer.Entities = append(outer.Entities, Entity{Type: "line", Start: &a, End: &b, Extents: &Extents{
			Left: math.Min(a.X, b.X), Right: math.Max(a.X, b.X), Bottom: math.Min(a.Y, b.Y), Top: math.Max(a.Y, b.Y),
		}})
	}

	return &Model{Cut: []Contour{outer}}
}

func withHole(m *Model, id string, c Point, dia float64) *Model {
	r := dia / 2
	ext := &Extents{Left: c.X - r, Right: c.X + r, Bottom: c.Y - r, Top: c.Y + r}
	m.Cut = append(m.Cut, Contour{ID: id, Closed: true, Circular: true, Area: math.Pi * r * r, Entities: []Entity{
		{Type: "arc", Center: &c, Radius: r, StartAngle: 0, EndAngle: 180, Extents: ext},
		{Type: "arc", Center: &c, Radius: r, StartAngle: 180, EndAngle: 360, Extents: ext},
	}})

	return m
}

func steel(t *testing.T) Material {
	t.Helper()

	var mats struct {
		Data []Material `json:"data"`
	}

	f := newFake(t)
	require.NoError(t, json.Unmarshal([]byte(f.fixture("materials.json")), &mats))

	for _, m := range mats.Data {
		if m.SKU == "CRS-074" {
			return m
		}
	}

	t.Fatal("no CRS-074 in the materials fixture")

	return Material{}
}

func TestTappingRules(t *testing.T) {
	m4 := map[string]TapSize{"M4 x 0.7": {Tap: "M4 x 0.7", Drill: 0.134}}
	tapM4 := ServicesInput{Taps: []TapInput{{HoleIDs: []string{"h"}, Tap: "M4 x 0.7"}}}

	t.Run("accepts a hole drawn at the tap's size, clear of the edges", func(t *testing.T) {
		pc := &partContext{drawing: &Drawing{ID: 1, Units: "in"}, model: withHole(squarePlate(), "h", Point{0.5, 0.5}, 0.134), material: steel(t), taps: m4}

		p, err := pc.plan(tapM4)

		require.NoError(t, err)
		assert.Equal(t, []TapConfig{{ID: "h", Tap: "M4 x 0.7"}}, p.taps)
		assert.Equal(t, map[string]int{"tapping": 1}, p.counts)
	})

	t.Run("accepts a smaller hole, which SendCutSend enlarges to the drill size", func(t *testing.T) {
		pc := &partContext{drawing: &Drawing{ID: 1, Units: "in"}, model: withHole(squarePlate(), "h", Point{0.5, 0.5}, 0.12), material: steel(t), taps: m4}

		_, err := pc.plan(tapM4)

		require.NoError(t, err)
	})

	t.Run("rejects a hole whose centre is too close to the edge for the tap", func(t *testing.T) {
		// Half the drill (0.067) plus CRS-074's minimum (0.022) is 0.089.
		pc := &partContext{drawing: &Drawing{ID: 1, Units: "in"}, model: withHole(squarePlate(), "h", Point{0.08, 0.5}, 0.134), material: steel(t), taps: m4}

		_, err := pc.plan(tapM4)

		require.ErrorContains(t, err, "hole h's centre is 0.080 in from the nearest edge; a M4 x 0.7 tap needs 0.089 in")
	})

	t.Run("measures against other holes too", func(t *testing.T) {
		m := withHole(withHole(squarePlate(), "h", Point{0.5, 0.5}, 0.134), "near", Point{0.6, 0.5}, 0.1)
		pc := &partContext{drawing: &Drawing{ID: 1, Units: "in"}, model: m, material: steel(t), taps: m4}

		_, err := pc.plan(tapM4)

		require.ErrorContains(t, err, "0.050 in from the nearest edge")
	})

	t.Run("rejects a tapped hole closer to a bend than half the bending die", func(t *testing.T) {
		m := withHole(squarePlate(), "h", Point{0.5, 0.5}, 0.134)
		a, b := Point{0.3, 0}, Point{0.3, 1}
		m.BendLines = []Entity{{ID: "b", Type: "line", Start: &a, End: &b}}
		pc := &partContext{drawing: &Drawing{ID: 1, Units: "in"}, model: m, material: steel(t), taps: m4}

		_, err := pc.plan(ServicesInput{
			Bends: []BendInput{{ID: "b", Angle: 90, Direction: "up"}},
			Taps:  tapM4.Taps,
		})

		require.ErrorContains(t, err, "hole h is 0.200 in from bend line b; tapped holes must be at least 0.236 in from bends")
	})

	t.Run("converts a drawing in millimetres", func(t *testing.T) {
		// 3.41 mm is an M4 hole; 4.5 mm from the edge is 0.177 in.
		pc := &partContext{drawing: &Drawing{ID: 1, Units: "mm"}, model: scaled(withHole(squarePlate(), "h", Point{4.5 / 25.4, 0.5}, 3.41/25.4), 25.4), material: steel(t), taps: m4}

		_, err := pc.plan(tapM4)

		require.NoError(t, err)
	})
}

// scaled multiplies a model's coordinates, to turn an inch drawing into a
// millimetre one.
func scaled(m *Model, k float64) *Model {
	sp := func(p *Point) *Point {
		if p == nil {
			return nil
		}

		return &Point{p.X * k, p.Y * k}
	}

	for i := range m.Cut {
		for j := range m.Cut[i].Entities {
			e := &m.Cut[i].Entities[j]
			e.Start, e.End, e.Center, e.Radius = sp(e.Start), sp(e.End), sp(e.Center), e.Radius*k

			if e.Extents != nil {
				e.Extents = &Extents{Top: e.Extents.Top * k, Right: e.Extents.Right * k, Bottom: e.Extents.Bottom * k, Left: e.Extents.Left * k}
			}
		}
	}

	return m
}

func TestFinishRules(t *testing.T) {
	finishes := []Finish{{Value: "matte_black", FinishType: "powder"}}

	t.Run("needs a cutout to hang the part from", func(t *testing.T) {
		pc := &partContext{drawing: &Drawing{ID: 1, Units: "in"}, model: squarePlate(), material: steel(t), finishes: finishes}

		_, err := pc.plan(ServicesInput{Powder: "matte_black"})

		require.ErrorContains(t, err, "powder needs a hole or cutout at least 0.063 in across")
	})

	t.Run("accepts a part with a big enough hole", func(t *testing.T) {
		pc := &partContext{drawing: &Drawing{ID: 1, Units: "in"}, model: withHole(squarePlate(), "h", Point{0.5, 0.5}, 0.1), material: steel(t), finishes: finishes}

		p, err := pc.plan(ServicesInput{Powder: "Matte_Black", Deburring: true})

		require.NoError(t, err)
		assert.Equal(t, []string{"powder", "deburring"}, p.operations)
		assert.Equal(t, map[string]string{"powder": "matte_black"}, p.finish)
	})
}

func TestSavedServicesGroupHolesByTap(t *testing.T) {
	got := savedServices(&SavedPart{
		Operations: []string{"tapping", "tumbling"},
		Tapping:    []TapConfig{{ID: "a", Tap: "6-32"}, {ID: "b", Tap: "M4 x 0.7"}, {ID: "c", Tap: "6-32"}},
	})

	assert.Equal(t, &ServicesInput{
		Taps:     []TapInput{{HoleIDs: []string{"a", "c"}, Tap: "6-32"}, {HoleIDs: []string{"b"}, Tap: "M4 x 0.7"}},
		Tumbling: true,
	}, got)
	assert.Nil(t, savedServices(&SavedPart{}), "nothing saved")
	assert.Nil(t, savedServices(nil))
}

func TestDistances(t *testing.T) {
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"to a segment's middle", segmentDistance(Point{1, 1}, Point{0, 0}, Point{2, 0}), 1},
		{"past a segment's end", segmentDistance(Point{3, 0}, Point{0, 0}, Point{2, 0}), 1},
		{"to a zero-length segment", segmentDistance(Point{3, 4}, Point{0, 0}, Point{0, 0}), 5},
		{"inside an arc's sweep", arcDistance(Point{0, 3}, Point{0, 0}, 1, 0, 180, false), 2},
		{"outside an arc's sweep, to its nearer end", arcDistance(Point{0, -3}, Point{0, 0}, 1, 0, 180, false), math.Hypot(1, 3)},
		{"a clockwise arc sweeps the other way", arcDistance(Point{0, -3}, Point{0, 0}, 1, 0, 180, true), 2},
		{"an arc across zero degrees", arcDistance(Point{3, 0}, Point{0, 0}, 1, 270, 90, false), 2},
		{"at an arc's centre", arcDistance(Point{0, 0}, Point{0, 0}, 1, 0, 90, false), 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.InDelta(t, tc.want, tc.got, 1e-9)
		})
	}
}
