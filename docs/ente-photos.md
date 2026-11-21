# Ente Photos on Lark

This integration uses one personal account on Ente's hosted service. It includes shared and hidden albums from that account. There is no album allowlist yet. The parent can hide the entire Ente library per companion. Scoped share links cannot use the library or see its slides.

## Set up on the bridge host

Use a bridge build containing this feature. Run `./scripts/setup-ente-photos.sh` from the checkout; normal host setup also accepts `./scripts/setup-host.sh --ente-photos`. The standalone script installs the checksum-pinned unmodified Ente Rust CLI 0.4.5 and writes the user units, but does not log in, start a service, or enable the timer. If the bridge was already running before you installed the helper for the first time, it will pick it up on its next normal start.

Log in yourself in a terminal on the bridge host. Do not paste your password, OTP or key into chat. Generate the vault key only once:

```bash
data=${XDG_DATA_HOME:-$HOME/.local/share}/lark
key=${XDG_CONFIG_HOME:-$HOME/.config}/lark/ente-vault-key
(
  umask 077
  if [[ ! -e "$key" ]]; then
    (set -C; "$data/bin/ente" vault key generate > "$key") || exit
  fi
  export ENTE_CLI_HOME="$data/ente-cli"
  ENTE_CLI_VAULT_KEY=$(<"$key")
  export ENTE_CLI_VAULT_KEY
  "$data/bin/ente" account login --account personal --app photos
)
```

The login is interactive. Use the same vault/key when reauthenticating; do not overwrite the key. The helper reads the mode-0600 key into **only** the CLI child's environment, not the unit file, bridge, or model prompt. After login:

```bash
systemctl --user daemon-reload
systemctl --user start lark-ente-photos.service
systemctl --user enable --now lark-ente-photos.timer
```

The first metadata sync can take a while for a large library. Later syncs reuse the CLI's local metadata replica. The bridge reconciles the published snapshot within about 15 seconds; missing photos are removed from its index and off active slides. Failed or partial syncs leave the last complete snapshot intact, and the library stops serving after 72 hours without a successful snapshot. Check `systemctl --user status lark-ente-photos.service` and the private `$data/ente-photos.log`. Do not share raw CLI logs: they can contain personal metadata.

Open the LAN `/app` with the bridge token and select **Photo Library**. Choose a companion to allow or hide the library. Search by title, caption, album or manually applied label. **Load preview** downloads only that still. Ask a local companion to show photos (e.g. “Show me photos of Grandma at the beach”) and use **Prev**, **Next** or **Exit**. A cache miss shows a loading screen while the bridge downloads one original. If the child is connected through a share link, the library is unavailable.

## Optional local models and backfill

Run `./scripts/setup-photo-models.sh` to install checksum-verified YuNet (MIT), SFace (Apache-2.0), and Xenova CLIP ViT-B/32 INT8 (MIT; OpenAI CLIP) with the tokenizer. Together they use about 193 MB. They use Lark's bundled ONNX Runtime in the next bridge process. If a model or runtime is unavailable, metadata and manual labels still work.

Newly viewed photos are analyzed locally. In **Photo Library → People**, review a cluster's cropped face previews, name the group, and confirm individual matches. A suggested face is never found by person name until you confirm it. Reject, split, or merge groups locally; a merge requires reconfirming moved faces. CLIP supplies scene results when metadata search leaves room. Names and embeddings stay on the host. The cloud model receives the child's request text, a short-lived turn access handle, and bounded photo handles, dates and counts—no original bytes, previews, file paths, or coordinates. The index includes the CLI's city catalog and raw location fields locally; city-to-file matching is not part of this version.

To process the library in the background, set `LARK_PHOTOS_BACKFILL=1` in the bridge environment before its next start. Do not enable it on a metered connection without checking the cost:

- Ente CLI 0.4.5 has no thumbnail download operation. Each eligible still or Live Photo archive is downloaded locally, decoded, turned into a metadata-free JPEG (≤512 px on its long edge), then deleted. Video, RAW, and inputs over 96 MiB or the decode limits are skipped. Orientation is applied once; HEIC's primary container transform takes priority over EXIF. Live Photos use the still and discard the movie.
- For 50,000 photos / 300 GB, allow up to about **300 GB of first-pass media traffic** if most originals are eligible, plus metadata and re-fetches. One transfer runs at a time. Backfill prefers favorites and newest photos, resumes from per-file status, and paces transfers at about 1 MiB/s on average. The CLI may burst faster and refreshes its replica before each download. Allow at least **80 hours** at the configured pacing, longer for CLI/network overhead. On-demand views also serialize but aren't paced.
- JPEG payload is capped at **1.5 GiB** (oldest generated previews are evicted). Scene vectors add about **98 MiB** at 50,000 photos, and each face adds 512 bytes of vector data plus row/index overhead. Allow a few GiB for the local `photos-index.db`, SQLite overhead, the metadata replica/snapshots, models and one transient original. Deletion/eviction removes rows; SQLite generally reuses pages rather than shrinking the file.
- The photo index is not in the bridge's backup. To remove all local derivatives, face/scene vectors and labels, stop the bridge, move its `photos-index.db` and any `photos-index.db-wal` / `photos-index.db-shm` files out of the data directory, and leave backfill disabled and models uninstalled if you do not want regeneration. Remove the separately managed Ente CLI home and snapshots only if you also want to disconnect Ente. Neither operation deletes cloud photos.

An independent full originals export is off by default. If wanted, run `./scripts/setup-ente-photos.sh --full-export`, reload user units, and run the same service again. It exports privately to `$data/ente-export/`, uses hardlinks where supported, and validates Ente's completion summary. Full exports retain removed items in `Trash/` per Ente's behavior and are an additional disk/network cost; the bridge still uses its independent small derivative cache.

Host smoke (Ryzen 7 7700X, ONNX Runtime 1.28.2 and two inference threads, synthetic media): the four models loaded in about 0.4 s, YuNet plus CLIP image inference averaged 45 ms/photo, SFace 15 ms/aligned face, and CLIP text 4 ms. The smoke process peaked at about 344 MiB RSS. Eight passes over a generated 12 MP JPEG averaged 195 ms for decode and derivative creation; its high-frequency pattern produced a 102 KiB preview. These are local processing measurements, not a logged-in Ente transfer test.

Sources: [Ente CLI 0.4.5](https://github.com/ente-io/ente/releases/tag/ente-cli-v0.4.5), [Photos](https://ente.com/help/cli/photos), [Accounts](https://ente.com/help/cli/accounts), [Automation](https://ente.com/help/cli/automation), [migration](https://ente.com/help/cli/migration), [YuNet](https://github.com/opencv/opencv_zoo/tree/main/models/face_detection_yunet), [SFace](https://github.com/opencv/opencv_zoo/tree/main/models/face_recognition_sface), [Xenova CLIP](https://huggingface.co/Xenova/clip-vit-base-patch32).
