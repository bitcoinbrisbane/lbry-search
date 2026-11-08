package claim

import (
	"encoding/hex"
	"testing"
)

// Vectors from lbry-sdk tests/unit/schema/test_claim_from_bytes.py.

func mustHex(t *testing.T, s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLegacyV1SignedYTSync(t *testing.T) {
	m := Decode(mustHex(t, "080110011aee04080112a604080410011a2b4865726520617265203520526561736f6e73204920e29da4e"+
		"fb88f204e657874636c6f7564207c20544c4722920346696e64206f7574206d6f72652061626f7574204e"+
		"657874636c6f75643a2068747470733a2f2f6e657874636c6f75642e636f6d2f0a0a596f752063616e206"+
		"6696e64206d65206f6e20746865736520736f6369616c733a0a202a20466f72756d733a2068747470733a"+
		"2f2f666f72756d2e6865617679656c656d656e742e696f2f0a202a20506f64636173743a2068747470733"+
		"a2f2f6f6666746f706963616c2e6e65740a202a2050617472656f6e3a2068747470733a2f2f7061747265"+
		"6f6e2e636f6d2f7468656c696e757867616d65720a202a204d657263683a2068747470733a2f2f7465657"+
		"37072696e672e636f6d2f73746f7265732f6f6666696369616c2d6c696e75782d67616d65720a202a2054"+
		"77697463683a2068747470733a2f2f7477697463682e74762f786f6e64616b0a202a20547769747465723"+
		"a2068747470733a2f2f747769747465722e636f6d2f7468656c696e757867616d65720a0a2e2e2e0a6874"+
		"7470733a2f2f7777772e796f75747562652e636f6d2f77617463683f763d4672546442434f535f66632a0"+
		"f546865204c696e75782047616d6572321c436f7079726967687465642028636f6e746163742061757468"+
		"6f722938004a2968747470733a2f2f6265726b2e6e696e6a612f7468756d626e61696c732f46725464424"+
		"34f535f666352005a001a41080110011a30040e8ac6e89c061f982528c23ad33829fd7146435bf7a4cc22"+
		"f0bff70c4fe0b91fd36da9a375e3e1c171db825bf5d1f32209766964656f2f6d70342a5c080110031a406"+
		"2b2dd4c45e364030fbfad1a6fefff695ebf20ea33a5381b947753e2a0ca359989a5cc7d15e5392a0d354c"+
		"0b68498382b2701b22c03beb8dcb91089031b871e72214feb61536c007cdf4faeeaab4876cb397feaf6b51"))
	if m.Type != Stream || m.Version != 1 {
		t.Fatalf("type %q version %d", m.Type, m.Version)
	}
	if m.Title != "Here are 5 Reasons I ❤️ Nextcloud | TLG" || m.Author != "The Linux Gamer" {
		t.Fatalf("title %q author %q", m.Title, m.Author)
	}
	if m.MediaType != "video/mp4" || m.Thumbnail != "https://berk.ninja/thumbnails/FrTdBCOS_fc" {
		t.Fatalf("media %q thumb %q", m.MediaType, m.Thumbnail)
	}
	if m.SDHash != "040e8ac6e89c061f982528c23ad33829fd7146435bf7a4cc22f0bff70c4fe0b91fd36da9a375e3e1c171db825bf5d1f3" {
		t.Fatalf("sd %s", m.SDHash)
	}
	if len(m.Languages) != 1 || m.Languages[0] != "en" {
		t.Fatalf("languages %v", m.Languages)
	}
	// The SDK reverses certificateId to get the channel hash; display order is the stored bytes.
	if m.ChannelID != "feb61536c007cdf4faeeaab4876cb397feaf6b51" {
		t.Fatalf("channel %s", m.ChannelID)
	}
}

func TestLegacyV1Channel(t *testing.T) {
	m := Decode(mustHex(t, "08011002225e0801100322583056301006072a8648ce3d020106052b8104000a034200043878b1edd4a13"+
		"73149909ef03f4339f6da9c2bd2214c040fd2e530463ffe66098eca14fc70b50ff3aefd106049a815f595"+
		"ed5a13eda7419ad78d9ed7ae473f17"))
	if m.Type != Channel {
		t.Fatalf("type %q", m.Type)
	}
}

func TestOldJSON(t *testing.T) {
	m := Decode([]byte(`{"license": "None", "author": "root", "language": "English", "title": "10MB speed test file", "content-type": "application/octet-stream", "thumbnail": "/home/robert/lbry/speed.jpg", "description": "10MB test file"}`))
	if m.Type != Stream || m.Title != "10MB speed test file" || m.Author != "root" || m.Languages[0] != "en" {
		t.Fatalf("%+v", m)
	}
}

// --- v2: built with a tiny encoder (same field numbers as claim.proto) ---

func pbVarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}
func pbBytes(num int, b []byte) []byte {
	return append(append(pbVarint(uint64(num<<3|2)), pbVarint(uint64(len(b)))...), b...)
}
func pbUint(num int, v uint64) []byte { return append(pbVarint(uint64(num<<3)), pbVarint(v)...) }
func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestV2SignedVideo(t *testing.T) {
	stream := cat(
		pbBytes(1, cat(pbBytes(4, []byte("video/mp4")), pbBytes(6, []byte{0xab, 0xcd}))),
		pbBytes(2, []byte("Jane")),
		pbUint(5, 1700000000),
		pbBytes(11, cat(pbUint(1, 1920), pbUint(2, 1080), pbUint(3, 95))),
	)
	claim := cat(
		pbBytes(1, stream),
		pbBytes(8, []byte("Sailing to Fraser Island")),
		pbBytes(9, []byte("Day one")),
		pbBytes(10, pbBytes(5, []byte("https://thumbs/x.jpg"))),
		pbBytes(11, []byte("sailing")), pbBytes(11, []byte("boats")),
		pbBytes(12, pbUint(1, 1)),
	)
	channel := make([]byte, 20)
	for i := range channel {
		channel[i] = byte(i + 1)
	}
	value := cat([]byte{0x01}, channel, make([]byte, 64), claim)
	m := Decode(value)
	if m.Type != Stream || m.Version != 2 || m.Title != "Sailing to Fraser Island" || m.Description != "Day one" {
		t.Fatalf("%+v", m)
	}
	if m.Author != "Jane" || m.MediaType != "video/mp4" || m.Duration != 95 || m.ReleaseTime != 1700000000 {
		t.Fatalf("%+v", m)
	}
	if m.Thumbnail != "https://thumbs/x.jpg" || len(m.Tags) != 2 || m.Tags[1] != "boats" || m.Languages[0] != "en" {
		t.Fatalf("%+v", m)
	}
	if m.ChannelID != "14131211100f0e0d0c0b0a090807060504030201" {
		t.Fatalf("channel %s", m.ChannelID)
	}

	unsigned := Decode(cat([]byte{0x00}, pbBytes(2, nil), pbBytes(8, []byte("@sailing"))))
	if unsigned.Type != Channel || unsigned.ChannelID != "" || unsigned.Title != "@sailing" {
		t.Fatalf("%+v", unsigned)
	}
	repostTarget := make([]byte, 20)
	repostTarget[0] = 0xff
	repost := Decode(cat([]byte{0x00}, pbBytes(4, pbBytes(1, repostTarget))))
	if repost.Type != Repost || repost.RepostOf != hexReversed(repostTarget) {
		t.Fatalf("%+v", repost)
	}
	if Decode([]byte{0x01, 0x02}).Type != Unknown || Decode(nil).Type != Unknown {
		t.Fatal("garbage should be Unknown")
	}
}
