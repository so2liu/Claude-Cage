package workbench

// Native browser screenshots contain no system pointer. Send the standard RFB
// Cursor shape so noVNC can draw a local, responsive pointer over the image.
// Black fill and a white outline remain visible on light and dark web pages.
func nativeCursorUpdate() []byte {
	const width, height = 16, 20
	shape := []string{
		"+",
		"++",
		"+#+",
		"+##+",
		"+###+",
		"+####+",
		"+#####+",
		"+######+",
		"+#######+",
		"+########+",
		"+#####+++++",
		"+##+##+",
		"+#+ +##+",
		"++  +##+",
		"+    +##+",
		"     +##+",
		"      ++",
	}
	pixels := make([]byte, width*height*4)
	mask := make([]byte, ((width+7)/8)*height)
	for y, row := range shape {
		for x, point := range row {
			if point == ' ' {
				continue
			}
			mask[y*((width+7)/8)+x/8] |= 0x80 >> (x % 8)
			if point == '+' {
				i := (y*width + x) * 4
				pixels[i], pixels[i+1], pixels[i+2] = 255, 255, 255
			}
		}
	}
	packet := []byte{0, 0, 0, 1}
	packet = append(packet, windowRFBRect(width, height, -239)...)
	packet = append(packet, pixels...)
	return append(packet, mask...)
}
