package sendcutsend

import "strings"

// testPlateDXF is a 2 by 3 inch plate with one half-inch hole, as a minimal
// R12 DXF: pairs of group code and value, one per line.
var testPlateDXF = strings.Join([]string{
	"0", "SECTION", "2", "HEADER", "9", "$INSUNITS", "70", "1", "0", "ENDSEC",
	"0", "SECTION", "2", "ENTITIES",
	"0", "LINE", "8", "0", "10", "0", "20", "0", "11", "2", "21", "0",
	"0", "LINE", "8", "0", "10", "2", "20", "0", "11", "2", "21", "3",
	"0", "LINE", "8", "0", "10", "2", "20", "3", "11", "0", "21", "3",
	"0", "LINE", "8", "0", "10", "0", "20", "3", "11", "0", "21", "0",
	"0", "CIRCLE", "8", "0", "10", "1", "20", "1.5", "40", "0.25",
	"0", "ENDSEC", "0", "EOF",
}, "\n") + "\n"
