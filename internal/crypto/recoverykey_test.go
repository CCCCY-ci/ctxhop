package crypto

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRecoveryKeyRoundTrip(t *testing.T) {
	raw, text := NewRecoveryKey()

	back, err := ParseRecoveryKey(text)
	if err != nil {
		t.Fatalf("ParseRecoveryKey(%q): %v", text, err)
	}
	if !bytes.Equal(raw, back) {
		t.Error("the key did not survive being written and read back")
	}
}

func TestRecoveryKeyIsWritableByHand(t *testing.T) {
	_, text := NewRecoveryKey()

	if !strings.HasPrefix(text, recoveryPrefix+"-") {
		t.Errorf("a key should be recognisable months later: %q", text)
	}
	// Grouped, so the eye can keep its place while copying.
	for _, group := range strings.Split(text, "-")[1:] {
		if len(group) > 4 {
			t.Errorf("group %q is too long to copy reliably", group)
		}
	}
	// None of the characters people confuse when writing by hand.
	for _, r := range strings.ReplaceAll(text, "-", "") {
		if strings.ContainsRune("ILOU", r) {
			t.Errorf("the alphabet includes %q, which is confused with another character", r)
		}
	}
}

func TestRecoveryKeyCatchesASingleMistypedCharacter(t *testing.T) {
	// The whole point of the checksum: a slip must be reported as a slip, not
	// surface later as "wrong key" and send the user looking elsewhere.
	_, text := NewRecoveryKey()
	compact := strings.ReplaceAll(text, "-", "")

	changed := 0
	for i := len(recoveryPrefix); i < len(compact); i++ {
		for _, replacement := range crockford {
			if rune(compact[i]) == replacement {
				continue
			}
			damaged := compact[:i] + string(replacement) + compact[i+1:]

			if _, err := ParseRecoveryKey(damaged); err == nil {
				t.Fatalf("a single wrong character at %d was accepted", i)
			}
			changed++
			break
		}
	}
	if changed == 0 {
		t.Fatal("nothing was tested")
	}
}

func TestRecoveryKeyToleratesHowPeopleActuallyType(t *testing.T) {
	raw, text := NewRecoveryKey()

	variants := map[string]string{
		"lowercase":       strings.ToLower(text),
		"no separators":   strings.ReplaceAll(text, "-", ""),
		"spaces instead":  strings.ReplaceAll(text, "-", " "),
		"extra spaces":    "  " + text + "  ",
		"underscores":     strings.ReplaceAll(text, "-", "_"),
		"mixed case":      strings.ToLower(text[:10]) + text[10:],
		"without prefix":  strings.TrimPrefix(text, recoveryPrefix+"-"),
		"letter l for 1":  strings.ReplaceAll(text, "1", "l"),
		"letter O for 0":  strings.ReplaceAll(text, "0", "O"),
		"capital I for 1": strings.ReplaceAll(text, "1", "I"),
	}

	for name, variant := range variants {
		t.Run(name, func(t *testing.T) {
			back, err := ParseRecoveryKey(variant)
			if err != nil {
				t.Fatalf("ParseRecoveryKey(%q): %v", variant, err)
			}
			if !bytes.Equal(raw, back) {
				t.Error("decoded to a different key")
			}
		})
	}
}

func TestParseRecoveryKeyRejectsRubbish(t *testing.T) {
	for name, input := range map[string]string{
		"empty":              "",
		"too short":          "AGSY-AB",
		"outside alphabet":   "AGSY-" + strings.Repeat("!", 56),
		"right shape wrong":  "AGSY-" + strings.Repeat("A", 56),
		"truncated but sane": func() string { _, t := NewRecoveryKey(); return t[:20] }(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRecoveryKey(input); err == nil {
				t.Errorf("ParseRecoveryKey(%q) succeeded", input)
			}
		})
	}
}

func TestRecoveryKeyChecksumErrorIsDistinct(t *testing.T) {
	// A mistyped key and a wrong key are different problems with different
	// remedies, so they must not report the same thing.
	_, text := NewRecoveryKey()
	compact := strings.ReplaceAll(text, "-", "")
	damaged := compact[:len(compact)-1] + "Z"
	if compact[len(compact)-1] == 'Z' {
		damaged = compact[:len(compact)-1] + "Y"
	}

	_, err := ParseRecoveryKey(damaged)
	if !errors.Is(err, ErrRecoveryKeyChecksum) {
		t.Errorf("got %v, want ErrRecoveryKeyChecksum", err)
	}
	if !strings.Contains(err.Error(), "mistyped") {
		t.Errorf("the message should tell the user what to look for: %v", err)
	}
}

func TestCrockfordRoundTrip(t *testing.T) {
	for _, data := range [][]byte{
		{},
		{0x00},
		{0xff},
		{0x00, 0x01, 0x02, 0x03, 0x04},
		bytes.Repeat([]byte{0xa5}, 32),
	} {
		encoded := encodeCrockford(data)
		back, err := decodeCrockford(encoded)
		if err != nil {
			t.Fatalf("decodeCrockford(%q): %v", encoded, err)
		}
		// Encoding pads to a five-bit boundary, so the tail may gain a byte;
		// what matters is that the original survives as a prefix.
		if !bytes.HasPrefix(back, data) {
			t.Errorf("round trip lost data: %x -> %q -> %x", data, encoded, back)
		}
	}
}

func TestDecodeCrockfordRejectsUnknownCharacters(t *testing.T) {
	if _, err := decodeCrockford("ABC!"); err == nil {
		t.Error("an out-of-alphabet character was accepted")
	}
}

func TestNewRecoveryKeysDiffer(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		_, text := NewRecoveryKey()
		if seen[text] {
			t.Fatal("a recovery key repeated")
		}
		seen[text] = true
	}
}
