package sendcutsend

import (
	"math"
)

// Point is a 2D point in a part's drawing units.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

func (p Point) sub(q Point) Point { return Point{p.X - q.X, p.Y - q.Y} }

func (p Point) dot(q Point) float64 { return p.X*q.X + p.Y*q.Y }

func (p Point) dist(q Point) float64 { return math.Hypot(p.X-q.X, p.Y-q.Y) }

// Extents is an axis-aligned bounding box.
type Extents struct {
	Top    float64 `json:"top"`
	Right  float64 `json:"right"`
	Bottom float64 `json:"bottom"`
	Left   float64 `json:"left"`
}

// Entity is one line or arc of a processed part's geometry. Arc angles are
// in degrees, counter-clockwise from +x, swept from StartAngle to EndAngle
// counter-clockwise unless Clockwise is set.
type Entity struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Start      *Point   `json:"start"`
	End        *Point   `json:"end"`
	Center     *Point   `json:"center"`
	Radius     float64  `json:"radius"`
	StartAngle float64  `json:"startAngle"`
	EndAngle   float64  `json:"endAngle"`
	Clockwise  bool     `json:"clockwise"`
	Extents    *Extents `json:"extents"`
}

// Contour is one closed outline: the part's outside edge or a cutout.
type Contour struct {
	ID       string   `json:"id"`
	Closed   bool     `json:"closed"`
	Circular bool     `json:"circular"`
	Area     float64  `json:"area"`
	Entities []Entity `json:"entities"`
}

// Model is the geometry SendCutSend extracts from a part's file: the cut
// outlines and the bend lines, in the drawing's units.
type Model struct {
	Cut       []Contour `json:"cut"`
	BendLines []Entity  `json:"bendLines"`
}

// distanceTo returns the distance from p to the entity, and false when the
// entity is of a kind this package can't measure.
func (e *Entity) distanceTo(p Point) (float64, bool) {
	switch e.Type {
	case "line":
		if e.Start == nil || e.End == nil {
			return 0, false
		}

		return segmentDistance(p, *e.Start, *e.End), true
	case "arc":
		if e.Center == nil {
			return 0, false
		}

		return arcDistance(p, *e.Center, e.Radius, e.StartAngle, e.EndAngle, e.Clockwise), true
	case "circle":
		if e.Center == nil {
			return 0, false
		}

		return math.Abs(p.dist(*e.Center) - e.Radius), true
	}

	return 0, false
}

// length returns a line's length, and false for any other entity.
func (e *Entity) length() (float64, bool) {
	if e.Type != "line" || e.Start == nil || e.End == nil {
		return 0, false
	}

	return e.Start.dist(*e.End), true
}

// segmentDistance is the distance from p to the segment a-b.
func segmentDistance(p, a, b Point) float64 {
	ab := b.sub(a)

	l2 := ab.dot(ab)
	if l2 == 0 {
		return p.dist(a)
	}

	t := math.Max(0, math.Min(1, p.sub(a).dot(ab)/l2))

	return p.dist(Point{a.X + t*ab.X, a.Y + t*ab.Y})
}

// arcDistance is the distance from p to an arc. When p's direction from the
// centre lies within the arc's sweep the nearest point is on the arc;
// otherwise it is one of the arc's ends.
func arcDistance(p, c Point, r, startDeg, endDeg float64, clockwise bool) float64 {
	start, end := startDeg, endDeg
	if clockwise {
		start, end = endDeg, startDeg
	}

	sweep := normDeg(end - start)
	if sweep == 0 && startDeg != endDeg {
		sweep = 360
	}

	d := p.dist(c)
	if d > 0 {
		at := normDeg(math.Atan2(p.Y-c.Y, p.X-c.X)*180/math.Pi - start)
		if at <= sweep {
			return math.Abs(d - r)
		}
	}

	ends := [2]Point{polar(c, r, startDeg), polar(c, r, endDeg)}

	return math.Min(p.dist(ends[0]), p.dist(ends[1]))
}

// normDeg maps an angle to [0, 360).
func normDeg(a float64) float64 {
	a = math.Mod(a, 360)
	if a < 0 {
		a += 360
	}

	return a
}

func polar(c Point, r, deg float64) Point {
	rad := deg * math.Pi / 180

	return Point{c.X + r*math.Cos(rad), c.Y + r*math.Sin(rad)}
}

// hole returns the centre and diameter of a circular contour.
func (c *Contour) hole() (Point, float64, bool) {
	if !c.Circular {
		return Point{}, 0, false
	}

	for _, e := range c.Entities {
		if (e.Type == "arc" || e.Type == "circle") && e.Center != nil && e.Radius > 0 {
			return *e.Center, 2 * e.Radius, true
		}
	}

	return Point{}, 0, false
}

// bounds returns the contour's bounding box from its entities' extents.
func (c *Contour) bounds() (Extents, bool) {
	var (
		b  Extents
		ok bool
	)

	for _, e := range c.Entities {
		if e.Extents == nil {
			continue
		}

		x := *e.Extents
		if !ok {
			b, ok = x, true

			continue
		}

		b.Top, b.Bottom = math.Max(b.Top, x.Top), math.Min(b.Bottom, x.Bottom)
		b.Right, b.Left = math.Max(b.Right, x.Right), math.Min(b.Left, x.Left)
	}

	return b, ok
}

// outer returns the index of the part's outside edge: the largest contour.
func (m *Model) outer() int {
	best := -1

	for i := range m.Cut {
		if best < 0 || m.Cut[i].Area > m.Cut[best].Area {
			best = i
		}
	}

	return best
}

// clearance is the distance from p to the nearest cut geometry other than
// the contour with id skip. ok is false when some geometry couldn't be
// measured, so the result may be too large.
func (m *Model) clearance(p Point, skip string) (dist float64, ok bool) {
	dist, ok = math.Inf(1), true

	for i := range m.Cut {
		c := &m.Cut[i]
		if c.ID == skip {
			continue
		}

		for j := range c.Entities {
			d, measured := c.Entities[j].distanceTo(p)
			if !measured {
				ok = false

				continue
			}

			dist = math.Min(dist, d)
		}
	}

	return dist, ok
}

// toInches converts a length in the drawing's units to inches.
func toInches(v float64, units string) float64 {
	if units == "mm" {
		return v / 25.4
	}

	return v
}
