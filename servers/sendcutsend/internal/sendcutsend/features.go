package sendcutsend

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	featuresDescription = "List a part's bend lines and round holes, with the IDs the services input of quote_part and create_saved_cart takes. With material_sku, also give each hole the taps that fit it in that material and the material's bend angle limits. saved_services is the part's saved configuration (what it was last ordered with, or the bends SendCutSend detected in a 3D file), ready to pass as services."
	finishesDescription = "List the powder coat, anodizing and plating options, with the values the services input takes. With material_sku, only the kinds that material offers."
	maxHoles            = 200
)

// FeaturesInput is get_part_features' input.
type FeaturesInput struct {
	PartID      int64  `json:"part_id" jsonschema:"the part's ID"`
	MaterialSKU string `json:"material_sku,omitempty" jsonschema:"check holes and bends against this material's rules"`
}

// FeaturesOutput is get_part_features' output.
type FeaturesOutput struct {
	PartID        int64          `json:"part_id"`
	Units         string         `json:"units" jsonschema:"the drawing's units; sizes below are in inches"`
	BendLines     []BendLineInfo `json:"bend_lines"`
	Holes         []HoleInfo     `json:"holes" jsonschema:"round holes, smallest first"`
	HolesOmitted  int            `json:"holes_omitted,omitempty" jsonschema:"round holes left out past the first 200"`
	BendLimits    *BendLimits    `json:"bend_limits,omitempty"`
	Services      []string       `json:"material_services,omitempty" jsonschema:"services the material offers"`
	SavedServices *ServicesInput `json:"saved_services,omitempty"`
}

// BendLineInfo is one bend line.
type BendLineInfo struct {
	ID       string  `json:"id"`
	LengthIn float64 `json:"length_in"`
	FromX    float64 `json:"from_x_in"`
	FromY    float64 `json:"from_y_in"`
	ToX      float64 `json:"to_x_in"`
	ToY      float64 `json:"to_y_in"`
}

// HoleInfo is one round hole.
type HoleInfo struct {
	ID          string   `json:"id"`
	DiameterIn  float64  `json:"diameter_in"`
	CenterX     float64  `json:"center_x_in"`
	CenterY     float64  `json:"center_y_in"`
	TapsThatFit []string `json:"taps_that_fit,omitempty" jsonschema:"with material_sku: taps this hole can take"`
}

// BendLimits are a material's bend rules.
type BendLimits struct {
	MinAngle          float64 `json:"min_angle"`
	MaxAngle          float64 `json:"max_angle"`
	MaxBendLengthIn   float64 `json:"max_bend_length_in"`
	MinFlangeLengthIn float64 `json:"min_flange_length_in"`
}

func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }

func (c *Client) featuresTool(ctx context.Context, _ *mcp.CallToolRequest, in FeaturesInput) (*mcp.CallToolResult, FeaturesOutput, error) {
	if err := checkID(in.PartID, "part_id"); err != nil {
		return nil, FeaturesOutput{}, err
	}

	s, err := c.newSession(ctx)
	if err != nil {
		return nil, FeaturesOutput{}, err
	}

	d, err := s.requireQuotable(ctx, in.PartID)
	if err != nil {
		return nil, FeaturesOutput{}, err
	}

	var mat Material

	sku := strings.ToUpper(strings.TrimSpace(in.MaterialSKU))
	if sku != "" {
		mats, err := s.materials(ctx)
		if err != nil {
			return nil, FeaturesOutput{}, err
		}

		i := slices.IndexFunc(mats, func(m Material) bool { return m.SKU == sku })
		if i < 0 {
			return nil, FeaturesOutput{}, fmt.Errorf("unknown material_sku %q; see list_materials", in.MaterialSKU)
		}

		mat = mats[i]
	}

	pc, err := s.partContext(ctx, d, mat, sku != "", false)
	if err != nil {
		return nil, FeaturesOutput{}, err
	}

	out := FeaturesOutput{
		PartID: d.ID, Units: d.Units, BendLines: []BendLineInfo{}, Holes: []HoleInfo{},
		SavedServices: savedServices(d.Part),
	}

	for _, b := range pc.model.BendLines {
		info := BendLineInfo{ID: b.ID}
		if l, ok := b.length(); ok {
			info.LengthIn = round4(pc.inches(l))
		}

		if b.Start != nil && b.End != nil {
			info.FromX, info.FromY = round4(pc.inches(b.Start.X)), round4(pc.inches(b.Start.Y))
			info.ToX, info.ToY = round4(pc.inches(b.End.X)), round4(pc.inches(b.End.Y))
		}

		out.BendLines = append(out.BendLines, info)
	}

	for i := range pc.model.Cut {
		h := &pc.model.Cut[i]

		center, dia, ok := h.hole()
		if !ok {
			continue
		}

		info := HoleInfo{
			ID: h.ID, DiameterIn: round4(pc.inches(dia)),
			CenterX: round4(pc.inches(center.X)), CenterY: round4(pc.inches(center.Y)),
		}

		if sku != "" && slices.Contains(mat.Services(), "tapping") {
			info.TapsThatFit = pc.tapsThatFit(h)
		}

		out.Holes = append(out.Holes, info)
	}

	sort.SliceStable(out.Holes, func(i, j int) bool { return out.Holes[i].DiameterIn < out.Holes[j].DiameterIn })

	if len(out.Holes) > maxHoles {
		out.HolesOmitted = len(out.Holes) - maxHoles
		out.Holes = out.Holes[:maxHoles]
	}

	if sku != "" {
		out.Services = mat.Services()

		var limits BendingLimits
		if mat.limits("bending", &limits) == nil {
			out.BendLimits = &BendLimits{
				MinAngle: float64(limits.MinAngle), MaxAngle: float64(limits.MaxAngle),
				MaxBendLengthIn: float64(limits.MaxWidth), MinFlangeLengthIn: float64(limits.MinFlangeLength),
			}
		}
	}

	return nil, out, nil
}

// FinishesInput is list_finishes' input.
type FinishesInput struct {
	MaterialSKU string `json:"material_sku,omitempty" jsonschema:"only the finishes this material offers"`
}

// FinishesOutput is list_finishes' output.
type FinishesOutput struct {
	Finishes []FinishInfo `json:"finishes"`
}

// FinishInfo is one finish option.
type FinishInfo struct {
	Kind  string `json:"kind" jsonschema:"powder, anodizing or plating: the services field to put the value in"`
	Value string `json:"value"`
	Color string `json:"color"`
	Code  string `json:"code,omitempty"`
}

func (c *Client) finishesTool(ctx context.Context, _ *mcp.CallToolRequest, in FinishesInput) (*mcp.CallToolResult, FinishesOutput, error) {
	s, err := c.newSession(ctx)
	if err != nil {
		return nil, FinishesOutput{}, err
	}

	kinds := finishOperations

	if sku := strings.ToUpper(strings.TrimSpace(in.MaterialSKU)); sku != "" {
		mats, err := s.materials(ctx)
		if err != nil {
			return nil, FinishesOutput{}, err
		}

		i := slices.IndexFunc(mats, func(m Material) bool { return m.SKU == sku })
		if i < 0 {
			return nil, FinishesOutput{}, fmt.Errorf("unknown material_sku %q; see list_materials", in.MaterialSKU)
		}

		offered := mats[i].Services()
		kinds = slices.DeleteFunc(slices.Clone(kinds), func(k string) bool { return !slices.Contains(offered, k) })
	}

	all, err := s.finishes(ctx)
	if err != nil {
		return nil, FinishesOutput{}, err
	}

	out := FinishesOutput{Finishes: []FinishInfo{}}

	for _, f := range all {
		if slices.Contains(kinds, f.FinishType) {
			out.Finishes = append(out.Finishes, FinishInfo{Kind: f.FinishType, Value: f.Value, Color: f.Color, Code: f.Code})
		}
	}

	return nil, out, nil
}
