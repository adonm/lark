package main

import (
	"context"
	"errors"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/chatmail/rpc-client-go/v2/deltachat"
	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

func decodeQRFromPNG(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return "", err
	}
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return "", err
	}
	res, err := qrcode.NewQRCodeReader().Decode(bmp, nil)
	if err != nil {
		return "", err
	}
	return res.GetText(), nil
}

type qaDCClient struct {
	tr     *deltachat.IOTransport
	rpc    *deltachat.Rpc
	accID  uint32
	dir    string
	paired []uint32
}

func startQADCClient(ctx context.Context, relay string) (*qaDCClient, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	local := filepath.Join(env("XDG_DATA_HOME", filepath.Join(home, ".local/share")), "lark")
	bin := findDCRPCBinary(local)
	if bin == "" {
		return nil, errors.New("deltachat-rpc-server not found")
	}
	if relay == "" {
		relay = "nine.testrun.org"
	}
	dir, err := os.MkdirTemp("", "lark-qa-dc-*")
	if err != nil {
		return nil, err
	}
	tr := deltachat.NewIOTransport()
	tr.Cmd = bin
	tr.AccountsDir = dir
	if err := tr.Open(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	rpc := &deltachat.Rpc{Context: ctx, Transport: tr}
	accID, err := rpc.AddAccount()
	if err != nil {
		tr.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	name := "QA Client"
	_ = rpc.SetConfig(accID, "displayname", &name)
	if err := rpc.AddTransportFromQr(accID, "dcaccount:"+relay); err != nil {
		tr.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := rpc.StartIoForAllAccounts(); err != nil {
		tr.Close()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("start IO: %w", err)
	}
	return &qaDCClient{tr: tr, rpc: rpc, accID: accID, dir: dir}, nil
}

func (q *qaDCClient) pairFromScreenPNG(pngPath string) (uint32, error) {
	qr, err := decodeQRFromPNG(pngPath)
	if err != nil {
		return 0, fmt.Errorf("decode QR from %s: %w", pngPath, err)
	}
	chatID, err := q.rpc.SecureJoin(q.accID, qr)
	if err != nil {
		return 0, err
	}
	q.paired = append(q.paired, chatID)
	return chatID, nil
}

func (q *qaDCClient) close() {
	if q == nil {
		return
	}
	if q.rpc != nil && q.accID != 0 {
		for _, chatID := range q.paired {
			if contacts, err := q.rpc.GetChatContacts(q.accID, chatID); err == nil {
				for _, cid := range contacts {
					_ = q.rpc.DeleteContact(q.accID, cid)
				}
			}
			_ = q.rpc.DeleteChat(q.accID, chatID)
		}
		_ = q.rpc.RemoveAccount(q.accID)
	}
	if q.tr != nil {
		q.tr.Close()
	}
	if q.dir != "" {
		_ = os.RemoveAll(q.dir)
	}
}

func TestDecodeQRFromPairPNG(t *testing.T) {
	t.Parallel()
	const wantQR = "https://i.delta.chat/#0123456789ABCDEF0123456789ABCDEF&a=qa%40example.org&n=Cake"
	img, err := renderPixelPair(pixelSpecForGeometry(roundScreen), "Cake", wantQR)
	if err != nil {
		t.Fatal(err)
	}
	pngBytes, err := imageToPNG(img)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pair.png")
	if err := os.WriteFile(path, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := decodeQRFromPNG(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantQR {
		t.Fatalf("decoded QR = %q, want %q", got, wantQR)
	}
}
