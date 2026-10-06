package tui

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestChromePaletteFor(t *testing.T) {
	if got := chromePaletteFor("nord").accent; got != "#88c0d0" {
		t.Errorf("nord accent = %q, want #88c0d0", got)
	}
	// Themes that render Markdown via a standard glamour style still get a chrome palette.
	if chromePaletteFor("dracula").accent == "" {
		t.Error("dracula should have a chrome palette")
	}
	// Unknown id and the default "dark" fall back to the default palette.
	if chromePaletteFor("dark").accent != defaultChromePalette.accent {
		t.Error("dark should use the default chrome palette")
	}
	if chromePaletteFor("nope").accent != defaultChromePalette.accent {
		t.Error("unknown theme should use the default chrome palette")
	}
}

func TestApplyChromeThemeRecolours(t *testing.T) {
	defer applyChromeTheme(defaultChromePalette) // don't leak global state to other tests

	applyChromeTheme(chromePaletteFor("nord"))
	nord := fmt.Sprint(logoStyle.GetForeground())
	applyChromeTheme(chromePaletteFor("gruvbox"))
	gruv := fmt.Sprint(logoStyle.GetForeground())
	if nord == gruv {
		t.Errorf("logo accent should differ between themes (got %q both)", nord)
	}

	// The configured accent matches the theme palette (profile-independent).
	applyChromeTheme(themePalette{accent: "#88c0d0", accent2: "#81a1c1", muted: "#4c566a"})
	if got := fmt.Sprint(logoStyle.GetForeground()); !strings.Contains(got, "88c0d0") {
		t.Errorf("logo foreground = %q, want the themed accent #88c0d0", got)
	}
	if got := fmt.Sprint(suggestStyle.GetForeground()); !strings.Contains(got, "81a1c1") {
		t.Errorf("suggest foreground = %q, want accent2 #81a1c1", got)
	}

	// Errors stay red (ANSI 9) regardless of theme (clarity).
	if got := fmt.Sprint(errStyle.GetForeground()); got != "9" {
		t.Errorf("errStyle foreground = %q, want 9 (red)", got)
	}
}

// relLuminance is WCAG 2's relative luminance of a #rrggbb colour.
func relLuminance(t *testing.T, hex string) float64 {
	t.Helper()
	var r, g, b int
	if _, err := fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b); err != nil {
		t.Fatalf("bad colour %q: %v", hex, err)
	}
	lin := func(c int) float64 {
		x := float64(c) / 255
		if x <= 0.03928 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func contrast(t *testing.T, a, b string) float64 {
	la, lb := relLuminance(t, a), relLuminance(t, b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// Every light theme is readable on its own background (#249): body text at
// WCAG AA (4.5), accents — headings, links, the logo, the prompt and
// type-ahead — at the large-text/UI floor (3), muted hints at 2.5 (they are
// meant to recede), and inline code on its code background at 3. These are
// what failed for the canonical colours this table substitutes.
func TestLightThemesAreLegible(t *testing.T) {
	light := 0
	for _, th := range renderThemes {
		p := th.palette
		if !p.light {
			continue
		}
		light++
		for _, c := range []struct {
			slot, fg, bg string
			min          float64
		}{
			{"fg", p.fg, p.bg, 4.5},
			{"accent", p.accent, p.bg, 3},
			{"accent2", p.accent2, p.bg, 3},
			{"accent3", p.accent3, p.bg, 3},
			{"muted", p.muted, p.bg, 2.5},
			{"inline code", p.accent3, p.codeBG, 3},
		} {
			if got := contrast(t, c.fg, c.bg); got < c.min {
				t.Errorf("%s: %s %s on %s has contrast %.2f, want >= %.1f", th.id, c.slot, c.fg, c.bg, got, c.min)
			}
		}
	}
	if light < 6 {
		t.Errorf("%d light themes, want the six of #249", light)
	}
}

func TestLightThemesResolveAndUseTheLightMarkdownBase(t *testing.T) {
	for name, id := range map[string]string{
		"latte": "catppuccin-latte", "Gruvbox Light": "gruvbox-light", "tokyo-day": "tokyo-night-day",
		"solarized": "solarized-light", "dawn": "rose-pine-dawn", "github": "light", "light": "light",
	} {
		th, ok := lookupTheme(name)
		if !ok || th.id != id {
			t.Errorf("lookupTheme(%q) = %q, %v; want %q", name, th.id, ok, id)
			continue
		}
		if !th.palette.light {
			t.Errorf("%s is not marked light", id)
		}
		if got := themeStyle(th.palette).Document.StylePrimitive.Color; got == nil || *got != th.palette.fg {
			t.Errorf("%s: document colour = %v, want %s", id, got, th.palette.fg)
		}
	}
	// The dark family names still mean the dark themes.
	for _, name := range []string{"gruvbox", "tokyo-night", "catppuccin", "mocha"} {
		if th, _ := lookupTheme(name); th.palette.light {
			t.Errorf("%q resolved to a light theme (%s)", name, th.id)
		}
	}
}

func TestDefaultThemeFollowsCOLORFGBG(t *testing.T) {
	for in, want := range map[string]string{
		"": "dark", "15;0": "dark", "0;15": "light", "0;7": "light", "12;8": "dark",
		"0;default;15": "light", "garbage": "dark",
	} {
		if got := defaultThemeID(in); got != want {
			t.Errorf("defaultThemeID(%q) = %q, want %q", in, got, want)
		}
	}
}
