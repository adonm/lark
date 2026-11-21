package main

import (
	"image"
	imagecolor "image/color"
	"os"
	"strings"
	"testing"
	"time"

	ort "github.com/yalue/onnxruntime_go"
)

func TestPhotoModelSmoke(t *testing.T) {
	dir, lib := os.Getenv("LARK_TEST_PHOTO_MODELS"), os.Getenv("LARK_TEST_PHOTO_ORT")
	if dir == "" || lib == "" {
		t.Skip("optional local photo models not configured")
	}
	start := time.Now()
	m, err := openPhotoML(dir, lib)
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	t.Logf("ONNX Runtime %s: opened four models in %v", ort.GetVersion(), time.Since(start))
	red := testJPEG(t, imagecolor.RGBA{R: 230, A: 255})
	blue := testJPEG(t, imagecolor.RGBA{B: 230, A: 255})
	var redVec, blueVec []float32
	start = time.Now()
	const samples = 12
	for i := range samples {
		img := red
		if i%2 == 1 {
			img = blue
		}
		vec, _, err := m.analyze(img)
		if err != nil || len(vec) != 512 {
			t.Fatal("invalid photo embedding:", err)
		}
		if i%2 == 0 {
			redVec = vec
		} else {
			blueVec = vec
		}
	}
	t.Logf("YuNet + CLIP image: %v/photo, %d synthetic images", time.Since(start)/samples, samples)
	start = time.Now()
	query, err := m.queryVector("a solid red color")
	if err != nil || len(query) != 512 {
		t.Fatal(err)
	}
	rSim, bSim := photoDot(query, encodePhotoVector(redVec)), photoDot(query, encodePhotoVector(blueVec))
	t.Logf("CLIP text: %v; red similarity %.3f, blue %.3f", time.Since(start), rSim, bSim)
	if !(rSim > bSim) {
		t.Fatal("CLIP did not rank the red fixture above blue")
	}
	fixture := image.NewRGBA(image.Rect(0, 0, 112, 112))
	f := photoFace{at: [5]image.Point{{38, 52}, {74, 52}, {56, 72}, {42, 92}, {71, 92}}}
	for _, pt := range f.at {
		fixture.Set(pt.X, pt.Y, imagecolor.White)
	}
	aligned := alignPhotoFace(fixture, f)
	if aligned == nil {
		t.Fatal("failed to align synthetic landmarks")
	}
	start = time.Now()
	for range samples {
		vec, err := runPhotoModel(m.sface, ort.Shape{1, 3, 112, 112}, photoChannels(aligned, false, false), ort.Shape{1, 128})
		if err != nil || len(clipVector(vec[0])) != 128 {
			t.Fatal("invalid SFace embedding", err)
		}
	}
	t.Logf("SFace: %v/aligned face", time.Since(start)/samples)
}

func TestPhotoPeopleRequireConfirmation(t *testing.T) {
	p := testLibrary(t)
	ctx, now := t.Context(), time.Now()
	ingestTestPhotos(t, p, fakeEnteSnapshot(), now)
	photos, err := p.search(ctx, "", "", 8)
	if err != nil || len(photos) < 2 {
		t.Fatal(err)
	}
	f := photoFace{rect: image.Rect(12, 12, 24, 24), vec: []float32{1, 0, 0}}
	for _, pkey := range photos {
		if err := p.storePhotoFeatures(ctx, pkey.photoKey, []float32{1, 0, 0}, []photoFace{f}); err != nil {
			t.Fatal(err)
		}
	}
	var cluster string
	var count int
	if err := p.db.QueryRow(`SELECT cluster,count(*) FROM library_face GROUP BY cluster`).Scan(&cluster, &count); err != nil || count != 2 {
		t.Fatalf("expected two proposed matches in one group: %d %v", count, err)
	}
	if _, err := p.changePerson(ctx, object{"cluster": cluster, "name": "Grandma"}, "person-name"); err != nil {
		t.Fatal(err)
	}
	matches, err := p.search(ctx, "", "Grandma", 8)
	if err != nil || len(matches) != 0 {
		t.Fatalf("unconfirmed faces leaked into person search: %v", err)
	}
	args, _ := decodeObject([]byte(`{"face":0}`))
	if _, err := p.changeFace(ctx, photos[0].photoKey, args, "face-confirm"); err != nil {
		t.Fatal(err)
	}
	matches, err = p.search(ctx, "", "Grandma", 8)
	if err != nil || len(matches) != 1 || matches[0].id != photos[0].id {
		t.Fatalf("confirmed photo was not found: %v", err)
	}
	matches, err = p.search(ctx, "Grandma beach", "Grandma", 8)
	if err != nil || len(matches) != 1 || matches[0].id != photos[0].id {
		t.Fatal("confirmed names cannot combine with metadata search", err)
	}
	if _, err := p.changeFace(ctx, photos[0].photoKey, args, "face-split"); err != nil {
		t.Fatal(err)
	}
	matches, err = p.search(ctx, "", "Grandma", 8)
	if err != nil || len(matches) > 0 {
		t.Fatal("split did not drop the previous match", err)
	}
	s := fakeEnteSnapshot()
	s.Files = s.Files[:0]
	ingestTestPhotos(t, p, s, now.Add(time.Second))
	for _, table := range []string{"library_person", "library_face", "library_vector"} {
		if err := p.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s retained deleted photo features: %d %v", strings.TrimPrefix(table, "library_"), count, err)
		}
	}
}
