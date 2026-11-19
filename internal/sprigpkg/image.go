package sprigpkg

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // decoders for pictures a sprig may have
	_ "image/jpeg"
	"image/png"
)

// maxPicturePixels bounds decoding a picture (a 4096×4096 image).
const maxPicturePixels = 4096 * 4096

// cleanPicture decodes a PNG, JPEG or GIF and encodes it again as PNG, so
// only pixels travel: no text chunks, EXIF or other metadata. Anything
// that can't be decoded within bounds is refused.
func cleanPicture(b []byte) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("not a PNG, JPEG or GIF picture: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPicturePixels {
		return nil, errors.New("the picture is too large (at most 4096×4096)")
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
