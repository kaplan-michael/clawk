package sandbox

import (
	"fmt"

	"github.com/clawkwork/clawk/machine/oci"
)

// layerBars maps an OCI download snapshot to one progress bar per layer.
func layerBars(downloads []oci.LayerStatus) []Bar {
	if len(downloads) == 0 {
		return nil
	}
	bars := make([]Bar, len(downloads))
	for i, d := range downloads {
		var frac float64
		switch {
		case d.CompressedSize > 0:
			frac = float64(d.Downloaded) / float64(d.CompressedSize)
		case d.Done:
			frac = 1
		}
		label := fmt.Sprintf("layer %d/%d", d.Index, len(downloads))
		if d.Cached {
			label += " (cached)"
		}
		bars[i] = Bar{Label: label, Frac: frac}
	}
	return bars
}
