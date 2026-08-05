// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package ipxe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"unicode/utf16"
)

const (
	// Ubuntu 24.04 ARM64 generic kernel and initrd URLs.
	defaultKernelURL = "https://cloud-images.ubuntu.com/releases/24.04/release/unpacked/ubuntu-24.04-server-cloudimg-arm64-vmlinuz-generic"
	defaultInitrdURL = "https://cloud-images.ubuntu.com/releases/24.04/release/unpacked/ubuntu-24.04-server-cloudimg-arm64-initrd-generic"

	// Names used for the replacement kernel/initrd inside /boot.
	replacementKernelName = "vmlinuz-generic"
	replacementInitrdName = "initrd-generic"

	// GPT constants.
	gptHeaderLBA     = 1
	gptSignature     = "EFI PART"
	sectorSize       = 512
	gptEntryMinBytes = 128
)

// nvidiaKernelPattern matches nvidia-64k kernel filenames in /boot and grub.cfg.
var nvidiaKernelPattern = regexp.MustCompile(`vmlinuz-\S*nvidia-64k`)

// nvidiaInitrdPattern matches nvidia-64k initrd filenames in /boot and grub.cfg.
var nvidiaInitrdPattern = regexp.MustCompile(`initrd\.img-\S*nvidia-64k`)

// BootPatcher modifies the /boot partition inside a qcow2 overlay to swap
// an incompatible kernel (e.g., nvidia-64k with 64KB pages) with a generic
// kernel that works on the host's hypervisor.
type BootPatcher struct {
	Logger      *slog.Logger // required
	CacheDir    string       // directory for cached kernel downloads
	DebugFSPath string       // path to debugfs binary (e2fsprogs)
	QEMUImgPath string       // path to qemu-img binary

	// KernelURL and InitrdURL override the default download locations.
	KernelURL string
	InitrdURL string

	// downloadFunc can be overridden for testing.
	downloadFunc func(ctx context.Context, url, dest string) error

	// bootCacheMu protects bootCacheInflight for concurrent boot partition
	// cache access. Multiple instances may provision simultaneously against the
	// same base image; the inflight map ensures only one extraction runs.
	bootCacheMu       sync.Mutex
	bootCacheInflight map[string]*bootCacheResult
}

// NeedsKernelSwap returns true if the host cannot boot the production
// nvidia-64k kernel. Currently: ARM64 on macOS (HVF uses 16KB pages,
// nvidia-64k needs 64KB).
func NeedsKernelSwap() bool {
	return runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
}

// PatchBoot modifies the /boot partition in the given qcow2 overlay:
//  1. Downloads a compatible kernel+initrd if not cached
//  2. Extracts the boot partition from the overlay to a temp file
//  3. Uses debugfs to replace vmlinuz, initrd, and patch grub.cfg
//  4. Writes the modified boot partition back into the overlay
func (p *BootPatcher) PatchBoot(ctx context.Context, overlayPath string) error {
	log := p.Logger
	log.Info("Starting kernel swap", "overlay", overlayPath)

	// Step 1: Ensure we have a compatible kernel and initrd cached.
	kernelPath, initrdPath, err := p.ensureKernelCached(ctx)
	if err != nil {
		return fmt.Errorf("bootpatch: ensure kernel cached: %w", err)
	}
	log.Info("Kernel files ready", "kernel", kernelPath, "initrd", initrdPath)

	// Step 2: Read the GPT from the overlay to locate the boot partition.
	gptData, err := p.readGPTFromOverlay(ctx, overlayPath)
	if err != nil {
		return fmt.Errorf("bootpatch: read GPT: %w", err)
	}

	bootStart, bootSectors, err := findBootPartitionWithLogger(gptData, log)
	if err != nil {
		return fmt.Errorf("bootpatch: find boot partition: %w", err)
	}
	log.Info("Found boot partition",
		"startLBA", bootStart, "sectors", bootSectors,
		"sizeMB", (bootSectors*sectorSize)/(1024*1024))

	// Step 3: Extract the boot partition to a temporary raw file.
	tmpDir, err := os.MkdirTemp("", "bootpatch-*")
	if err != nil {
		return fmt.Errorf("bootpatch: create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	bootRaw := filepath.Join(tmpDir, "boot.raw")
	log.Info("Extracting boot partition", "overlay", overlayPath, "bootStart", bootStart, "bootSectors", bootSectors)
	if err := p.extractPartition(ctx, overlayPath, bootRaw, bootStart, bootSectors); err != nil {
		return fmt.Errorf("bootpatch: extract boot partition: %w", err)
	}

	// Step 4: Use debugfs to modify the boot partition.
	log.Info("Patching boot partition", "kernel", kernelPath, "initrd", initrdPath)
	if err := p.patchBootPartition(ctx, bootRaw, kernelPath, initrdPath, tmpDir); err != nil {
		return fmt.Errorf("bootpatch: patch boot partition: %w", err)
	}

	// Step 5: Write the modified boot partition back into the overlay.
	log.Info("Writing modified boot partition back into overlay", "overlay", overlayPath)
	if err := p.writePartition(ctx, bootRaw, overlayPath, bootStart, bootSectors); err != nil {
		return fmt.Errorf("bootpatch: write boot partition back: %w", err)
	}

	log.Info("Boot patcher: kernel swap complete", "overlay", overlayPath)
	return nil
}

// PatchGrubOnly modifies only grub.cfg inside the boot partition without
// swapping the kernel. Injects the datasource and console settings from opts
// into all kernel cmdlines.
//
// The expensive boot partition extraction (qemu-img dd through the qcow2
// backing chain) is cached per base image. Fresh qcow2 overlays use
// copy-on-write and haven't diverged from the backing image in the boot
// partition area, so the cached raw partition is byte-identical to what we'd
// extract from any overlay of that base image. Subsequent calls copy the
// cached file (fast sequential I/O) instead of re-extracting.
func (p *BootPatcher) PatchGrubOnly(ctx context.Context, overlayPath, baseImagePath string, opts GrubPatchOptions) error {
	log := p.Logger
	log.Info("Patching grub.cfg for VM compatibility", "overlay", overlayPath)

	// Get or populate the boot partition cache for this base image.
	// The first call extracts the partition and caches it; subsequent calls
	// reuse the cached extraction, avoiding the slow qemu-img dd read
	// through the qcow2 backing chain.
	entry, err := p.getOrPopulateBootCache(ctx, overlayPath, baseImagePath)
	if err != nil {
		return fmt.Errorf("bootpatch: boot partition cache: %w", err)
	}

	grubCfg, err := os.ReadFile(entry.grubCfgPath)
	if err != nil {
		return fmt.Errorf("bootpatch: read cached grub.cfg: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "bootpatch-*")
	if err != nil {
		return fmt.Errorf("bootpatch: create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Copy the cached boot partition to a working copy. Each instance needs
	// its own copy because the per-instance datasource URL gets written into
	// grub.cfg via debugfs, which modifies the raw partition in place.
	bootRaw := filepath.Join(tmpDir, "boot.raw")
	log.Info("Copying cached boot partition to working copy", "src", entry.bootRawPath, "dst", bootRaw)
	if err := copyFile(entry.bootRawPath, bootRaw); err != nil {
		return fmt.Errorf("bootpatch: copy cached boot.raw: %w", err)
	}

	// Apply per-instance grub.cfg modifications (datasource URL, console settings).
	// This is always instance-specific because the datasource URL contains the
	// instance ID (e.g., ds=nocloud-net;s=http://host/nocloud/<instanceID>/).
	newGrubCfg := PatchGrubCfg(string(grubCfg), "", opts)
	patchedPath := filepath.Join(tmpDir, "grub-patched.cfg")
	if err := os.WriteFile(patchedPath, []byte(newGrubCfg), 0o644); err != nil {
		return fmt.Errorf("bootpatch: write patched grub.cfg: %w", err)
	}

	// Write the patched grub.cfg into the working boot partition via debugfs.
	script := "rm grub/grub.cfg\n" + fmt.Sprintf("write %s grub/grub.cfg\n", patchedPath)
	scriptPath := filepath.Join(tmpDir, "debugfs-cmds.txt")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		return fmt.Errorf("bootpatch: write debugfs script: %w", err)
	}

	log.Info("Running debugfs to write patched grub.cfg", "bootRaw", bootRaw)
	cmd := exec.CommandContext(ctx, p.DebugFSPath, "-w", "-f", scriptPath, bootRaw)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bootpatch: debugfs modify: %w\noutput: %s", err, out)
	}
	log.Info("Patched grub.cfg", "output", string(out))

	// Write the modified boot partition back into the instance's overlay.
	// This only allocates new clusters in the overlay (COW); the backing
	// base image is never touched.
	log.Info("Writing modified boot partition back into overlay", "overlay", overlayPath)
	if err := p.writePartition(ctx, bootRaw, overlayPath, entry.startLBA, entry.sectors); err != nil {
		return fmt.Errorf("bootpatch: write boot partition back: %w", err)
	}

	log.Info("Grub.cfg patched for VM compatibility", "overlay", overlayPath)
	return nil
}

// ---------------------------------------------------------------------------
// Boot partition cache
//
// The boot partition inside a fresh qcow2 overlay is byte-identical to the
// backing base image's because no writes have been made to that region yet
// (qcow2 copy-on-write). Extracting it via qemu-img dd is expensive — it must
// parse L1/L2 tables, decompress clusters, and follow the backing chain.
//
// We cache the extracted raw partition, the original grub.cfg, and the GPT
// layout (startLBA, sectors) per base image. On subsequent calls, a plain file
// copy replaces the qcow2 extraction — significantly faster sequential I/O
// with no qcow2 overhead.
//
// Cache layout on disk:
//
//	<CacheDir>/boot-partitions/<sha256(baseImagePath)[:32]>/
//	    boot.raw      — raw ext4 boot partition
//	    grub.cfg      — original grub.cfg from the base image
//	    layout        — text: "<startLBA> <sectors>\n"
//	    .complete     — empty marker; written last for atomicity
// ---------------------------------------------------------------------------

// bootCacheEntry holds paths and metadata for a cached boot partition.
type bootCacheEntry struct {
	dir         string // cache directory for this base image
	bootRawPath string // path to cached raw boot partition
	grubCfgPath string // path to cached original grub.cfg
	startLBA    uint64 // boot partition start offset in the disk image
	sectors     uint64 // boot partition size in sectors
}

// bootCacheResult tracks an in-progress cache population for inflight dedup.
type bootCacheResult struct {
	done chan struct{}
	err  error
}

// getOrPopulateBootCache returns cached boot partition data for the given base
// image. On a cache miss it extracts the partition from the overlay and saves
// it for future reuse. Concurrent callers for the same base image share a
// single extraction via the inflight map.
func (p *BootPatcher) getOrPopulateBootCache(ctx context.Context, overlayPath, baseImagePath string) (*bootCacheEntry, error) {
	cacheDir := p.bootCacheDirForImage(baseImagePath)
	markerPath := filepath.Join(cacheDir, ".complete")

	// Fast path: cache already populated.
	if _, err := os.Stat(markerPath); err == nil {
		entry, readErr := readBootCacheEntry(cacheDir)
		if readErr == nil {
			p.Logger.Info("Boot partition cache hit", "baseImage", baseImagePath)
			return entry, nil
		}
		// Cache looks corrupted (e.g. partial write from a killed process).
		// Remove it and fall through to re-extract.
		p.Logger.Warn("Boot partition cache corrupted, re-extracting", "error", readErr)
		os.RemoveAll(cacheDir)
	}

	// Cache miss — use inflight dedup so concurrent provisions of the same
	// base image share a single extraction instead of racing.
	p.bootCacheMu.Lock()
	if p.bootCacheInflight == nil {
		p.bootCacheInflight = make(map[string]*bootCacheResult)
	}
	if result, ok := p.bootCacheInflight[cacheDir]; ok {
		p.bootCacheMu.Unlock()
		// Another goroutine is already extracting; wait for it.
		select {
		case <-result.done:
			if result.err != nil {
				return nil, result.err
			}
			return readBootCacheEntry(cacheDir)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	result := &bootCacheResult{done: make(chan struct{})}
	p.bootCacheInflight[cacheDir] = result
	p.bootCacheMu.Unlock()

	// Perform extraction and cache population.
	entry, extractErr := p.populateBootCache(ctx, overlayPath, cacheDir)
	result.err = extractErr
	close(result.done)

	// Clean up inflight entry so future misses (e.g. after cache eviction)
	// trigger a fresh extraction.
	p.bootCacheMu.Lock()
	delete(p.bootCacheInflight, cacheDir)
	p.bootCacheMu.Unlock()

	if extractErr != nil {
		return nil, extractErr
	}
	return entry, nil
}

// populateBootCache extracts the boot partition from the overlay, dumps the
// original grub.cfg, and writes everything to cacheDir. The .complete marker
// is written last so a partially-written cache is never treated as valid.
func (p *BootPatcher) populateBootCache(ctx context.Context, overlayPath, cacheDir string) (*bootCacheEntry, error) {
	log := p.Logger
	log.Info("Boot partition cache miss, extracting", "overlay", overlayPath, "cacheDir", cacheDir)

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create boot cache dir: %w", err)
	}

	// Read GPT to locate the boot partition.
	gptData, err := p.readGPTFromOverlay(ctx, overlayPath)
	if err != nil {
		return nil, fmt.Errorf("read GPT: %w", err)
	}
	startLBA, sectors, err := findBootPartitionWithLogger(gptData, log)
	if err != nil {
		return nil, fmt.Errorf("find boot partition: %w", err)
	}

	// Extract the raw boot partition into the cache directory.
	bootRawPath := filepath.Join(cacheDir, "boot.raw")
	log.Info("Extracting boot partition", "overlay", overlayPath, "startLBA", startLBA, "sectors", sectors,
		"sizeMB", (sectors*sectorSize)/(1024*1024))
	if err := p.extractPartition(ctx, overlayPath, bootRawPath, startLBA, sectors); err != nil {
		return nil, fmt.Errorf("extract boot partition: %w", err)
	}

	// Dump the original grub.cfg from the ext4 partition.
	grubCfgPath := filepath.Join(cacheDir, "grub.cfg")
	log.Info("Dumping grub.cfg from boot partition", "bootRaw", bootRawPath)
	if _, err := p.runDebugFS(ctx, bootRawPath, fmt.Sprintf("dump grub/grub.cfg %s", grubCfgPath)); err != nil {
		return nil, fmt.Errorf("dump grub.cfg: %w", err)
	}

	// Persist the partition layout so we know where to write back.
	layoutData := fmt.Sprintf("%d %d\n", startLBA, sectors)
	if err := os.WriteFile(filepath.Join(cacheDir, "layout"), []byte(layoutData), 0o644); err != nil {
		return nil, fmt.Errorf("write layout: %w", err)
	}

	// Marker written last — if the process dies before this point, the cache
	// directory won't have .complete and will be treated as a miss next time.
	if err := os.WriteFile(filepath.Join(cacheDir, ".complete"), nil, 0o644); err != nil {
		return nil, fmt.Errorf("write cache marker: %w", err)
	}

	log.Info("Boot partition cached", "startLBA", startLBA, "sectors", sectors)
	return &bootCacheEntry{
		dir:         cacheDir,
		bootRawPath: bootRawPath,
		grubCfgPath: grubCfgPath,
		startLBA:    startLBA,
		sectors:     sectors,
	}, nil
}

// bootCacheDirForImage returns the cache directory for a given base image.
// The directory name is derived from a hash of the base image path, which is
// itself content-addressed (ImageCache uses sha256(url) for filenames), so the
// cache naturally invalidates when the image URL changes.
func (p *BootPatcher) bootCacheDirForImage(baseImagePath string) string {
	h := sha256.Sum256([]byte(baseImagePath))
	return filepath.Join(p.CacheDir, "boot-partitions", fmt.Sprintf("%x", h[:16]))
}

// readBootCacheEntry reads the cached boot partition metadata from disk.
func readBootCacheEntry(cacheDir string) (*bootCacheEntry, error) {
	layoutData, err := os.ReadFile(filepath.Join(cacheDir, "layout"))
	if err != nil {
		return nil, fmt.Errorf("read layout: %w", err)
	}
	var startLBA, sectors uint64
	if _, err := fmt.Sscanf(string(layoutData), "%d %d", &startLBA, &sectors); err != nil {
		return nil, fmt.Errorf("parse layout: %w", err)
	}
	return &bootCacheEntry{
		dir:         cacheDir,
		bootRawPath: filepath.Join(cacheDir, "boot.raw"),
		grubCfgPath: filepath.Join(cacheDir, "grub.cfg"),
		startLBA:    startLBA,
		sectors:     sectors,
	}, nil
}

// copyFile performs a plain file copy from src to dst. Used to create a working
// copy of the cached boot partition that can be modified without corrupting the
// cache. A raw file copy is significantly faster than qemu-img dd through a
// qcow2 backing chain because it's simple sequential I/O with no cluster
// decompression or L1/L2 table lookups.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ensureKernelCached downloads the generic kernel and initrd if not already present.
func (p *BootPatcher) ensureKernelCached(ctx context.Context) (kernelPath, initrdPath string, err error) {
	log := p.Logger

	if err := os.MkdirAll(p.CacheDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create cache dir: %w", err)
	}

	kernelURL := p.KernelURL
	if kernelURL == "" {
		kernelURL = defaultKernelURL
	}
	initrdURL := p.InitrdURL
	if initrdURL == "" {
		initrdURL = defaultInitrdURL
	}

	kernelPath = filepath.Join(p.CacheDir, replacementKernelName)
	initrdPath = filepath.Join(p.CacheDir, replacementInitrdName)

	dl := p.downloadFunc
	if dl == nil {
		dl = httpDownload
	}

	// Download kernel if not cached.
	if _, err := os.Stat(kernelPath); os.IsNotExist(err) {
		log.Info("Boot patcher: downloading generic kernel", "url", kernelURL)
		if err := dl(ctx, kernelURL, kernelPath); err != nil {
			return "", "", fmt.Errorf("download kernel: %w", err)
		}
	}

	// Note: we do NOT download a replacement initrd. The production initrd
	// contains LUKS + LVM modules needed to unlock root. The generic kernel
	// is compatible with the production initrd (same ARM64 ABI).

	return kernelPath, initrdPath, nil
}

// httpDownload downloads a URL to dest with atomic rename.
func httpDownload(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}

	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// readGPTFromOverlay reads the first 34 sectors (GPT header + entries) from a qcow2.
func (p *BootPatcher) readGPTFromOverlay(ctx context.Context, overlayPath string) ([]byte, error) {
	// Read 34 sectors: 1 protective MBR + 1 GPT header + 32 partition entries.
	tmpDir, err := os.MkdirTemp("", "gpt-read-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	gptRaw := filepath.Join(tmpDir, "gpt.raw")
	args := []string{
		"dd",
		"if=" + overlayPath,
		"of=" + gptRaw,
		fmt.Sprintf("bs=%d", sectorSize),
		"count=34",
		"skip=0",
	}

	cmd := exec.CommandContext(ctx, p.QEMUImgPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("qemu-img dd (read GPT): %w\noutput: %s", err, out)
	}

	return os.ReadFile(gptRaw)
}

// gptPartitionEntry represents a single GPT partition entry.
type gptPartitionEntry struct {
	TypeGUID [16]byte
	StartLBA uint64
	EndLBA   uint64 // inclusive
	Name     string
}

// findBootPartition parses GPT data and returns the start LBA and sector count
// of the boot partition (partition 2, the ext4 /boot partition).
func findBootPartition(gptData []byte) (startLBA, sectors uint64, err error) {
	return findBootPartitionWithLogger(gptData, nil)
}

func findBootPartitionWithLogger(gptData []byte, log *slog.Logger) (startLBA, sectors uint64, err error) {
	if log == nil {
		log = slog.Default().With("component", "bootpatcher")
	}

	if len(gptData) < (gptHeaderLBA+1)*sectorSize+gptEntryMinBytes {
		return 0, 0, fmt.Errorf("GPT data too short: %d bytes", len(gptData))
	}

	// GPT header starts at LBA 1 (offset 512).
	hdrOff := gptHeaderLBA * sectorSize
	sig := string(gptData[hdrOff : hdrOff+8])
	if sig != gptSignature {
		return 0, 0, fmt.Errorf("invalid GPT signature: %q", sig)
	}

	// Parse header fields.
	partEntryStartLBA := binary.LittleEndian.Uint64(gptData[hdrOff+72 : hdrOff+80])
	numPartEntries := binary.LittleEndian.Uint32(gptData[hdrOff+80 : hdrOff+84])
	partEntrySize := binary.LittleEndian.Uint32(gptData[hdrOff+84 : hdrOff+88])

	if partEntrySize < gptEntryMinBytes {
		return 0, 0, fmt.Errorf("partition entry size too small: %d", partEntrySize)
	}

	entries := parseGPTEntries(gptData, partEntryStartLBA, numPartEntries, partEntrySize)

	// Find partition 2 (index 1) — the /boot partition.
	// We look for the second non-empty partition, or specifically index 1.
	if len(entries) < 2 {
		return 0, 0, fmt.Errorf("found only %d partition entries, need at least 2", len(entries))
	}

	boot := entries[1]
	if boot.StartLBA == 0 && boot.EndLBA == 0 {
		return 0, 0, fmt.Errorf("partition 2 is empty")
	}

	// EndLBA is inclusive, so sector count = end - start + 1.
	sectors = boot.EndLBA - boot.StartLBA + 1
	log.Info("Boot patcher: identified boot partition",
		"name", boot.Name, "startLBA", boot.StartLBA, "endLBA", boot.EndLBA, "sectors", sectors)

	return boot.StartLBA, sectors, nil
}

// parseGPTEntries extracts partition entries from raw GPT data.
func parseGPTEntries(data []byte, startLBA uint64, count, entrySize uint32) []gptPartitionEntry {
	baseOffset := startLBA * sectorSize
	var entries []gptPartitionEntry

	for i := uint32(0); i < count; i++ {
		off := baseOffset + uint64(i)*uint64(entrySize)
		if off+uint64(entrySize) > uint64(len(data)) {
			break
		}
		entry := data[off : off+uint64(entrySize)]

		// Check if entry is empty (all-zero type GUID).
		var typeGUID [16]byte
		copy(typeGUID[:], entry[0:16])
		allZero := true
		for _, b := range typeGUID {
			if b != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			continue
		}

		startLBA := binary.LittleEndian.Uint64(entry[32:40])
		endLBA := binary.LittleEndian.Uint64(entry[40:48])

		// Parse UTF-16LE name from bytes 56..128.
		name := parseUTF16LEName(entry[56:entrySize])

		entries = append(entries, gptPartitionEntry{
			TypeGUID: typeGUID,
			StartLBA: startLBA,
			EndLBA:   endLBA,
			Name:     name,
		})
	}

	return entries
}

// parseUTF16LEName decodes a null-terminated UTF-16LE partition name.
func parseUTF16LEName(raw []byte) string {
	// Each character is 2 bytes, little-endian.
	var runes []rune
	for i := 0; i+1 < len(raw); i += 2 {
		ch := uint16(raw[i]) | uint16(raw[i+1])<<8
		if ch == 0 {
			break
		}
		runes = append(runes, rune(ch))
	}
	return string(runes)
}

// encodeUTF16LE encodes a string as null-terminated UTF-16LE bytes.
func encodeUTF16LE(s string) []byte {
	codes := utf16.Encode([]rune(s))
	buf := make([]byte, (len(codes)+1)*2) // +1 for null terminator
	for i, c := range codes {
		buf[i*2] = byte(c)
		buf[i*2+1] = byte(c >> 8)
	}
	return buf
}

// extractPartition uses qemu-img dd to extract a partition from the overlay.
func (p *BootPatcher) extractPartition(ctx context.Context, overlayPath, destPath string, startLBA, sectors uint64) error {
	args := []string{
		"dd",
		"if=" + overlayPath,
		"of=" + destPath,
		fmt.Sprintf("bs=%d", sectorSize),
		fmt.Sprintf("count=%d", sectors),
		fmt.Sprintf("skip=%d", startLBA),
	}

	cmd := exec.CommandContext(ctx, p.QEMUImgPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img dd (extract): %w\noutput: %s", err, out)
	}
	return nil
}

// writePartition uses qemu-io to write a raw partition back into the overlay.
func (p *BootPatcher) writePartition(ctx context.Context, srcPath, overlayPath string, startLBA, sectors uint64) error {
	offset := startLBA * sectorSize
	info, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("stat source partition: %w", err)
	}
	size := info.Size()

	// qemu-io "write -s <file> <offset> <length>" writes from a source file.
	// Find qemu-io next to qemu-img, or fall back to PATH lookup.
	qemuIO := filepath.Join(filepath.Dir(p.QEMUImgPath), "qemu-io")
	if _, err := exec.LookPath(qemuIO); err != nil {
		qemuIO = "qemu-io" // fall back to PATH
	}
	writeCmd := fmt.Sprintf("write -s %s %d %d", srcPath, offset, size)

	cmd := exec.CommandContext(ctx, qemuIO, "-f", "qcow2", "-c", writeCmd, overlayPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-io write: %w\noutput: %s", err, out)
	}
	return nil
}

// patchBootPartition uses debugfs to replace the kernel, initrd, and grub.cfg
// inside the ext4 boot partition image.
func (p *BootPatcher) patchBootPartition(ctx context.Context, bootRaw, kernelPath, _, tmpDir string) error {
	log := p.Logger

	// Step 1: List files in /boot root to find the nvidia-64k kernel/initrd names.
	lsOutput, err := p.runDebugFS(ctx, bootRaw, "ls -l /")
	if err != nil {
		return fmt.Errorf("debugfs ls: %w", err)
	}

	oldKernel := findFilenameByPattern(lsOutput, nvidiaKernelPattern)
	oldInitrd := findFilenameByPattern(lsOutput, nvidiaInitrdPattern)
	if oldKernel == "" {
		return fmt.Errorf("no nvidia-64k kernel found in boot partition (ls output: %s)", lsOutput)
	}
	if oldInitrd == "" {
		return fmt.Errorf("no nvidia-64k initrd found in boot partition (ls output: %s)", lsOutput)
	}
	log.Info("Boot patcher: found existing kernel files",
		"kernel", oldKernel, "initrd", oldInitrd)

	// Step 2: Dump grub.cfg so we can patch it.
	grubCfgPath := filepath.Join(tmpDir, "grub.cfg")
	_, err = p.runDebugFS(ctx, bootRaw, fmt.Sprintf("dump grub/grub.cfg %s", grubCfgPath))
	if err != nil {
		return fmt.Errorf("debugfs dump grub.cfg: %w", err)
	}

	grubCfg, err := os.ReadFile(grubCfgPath)
	if err != nil {
		return fmt.Errorf("read grub.cfg: %w", err)
	}

	// Step 3: Patch grub.cfg to reference the new kernel/initrd.
	newGrubCfg := PatchGrubCfg(string(grubCfg), oldKernel, DefaultGrubPatchOptions())
	patchedGrubPath := filepath.Join(tmpDir, "grub-patched.cfg")
	if err := os.WriteFile(patchedGrubPath, []byte(newGrubCfg), 0o644); err != nil {
		return fmt.Errorf("write patched grub.cfg: %w", err)
	}

	// Step 4: Build a debugfs command script to perform all modifications.
	// We only swap the kernel (vmlinuz), keeping the original initrd intact.
	// The production initrd contains LUKS + LVM modules needed to unlock root.
	// The generic kernel is compatible with the nvidia-64k initrd.
	// debugfs -w -f <script> <device>
	script := strings.Join([]string{
		fmt.Sprintf("rm %s", oldKernel),
		fmt.Sprintf("write %s %s", kernelPath, replacementKernelName),
		"rm grub/grub.cfg",
		fmt.Sprintf("write %s grub/grub.cfg", patchedGrubPath),
	}, "\n")

	scriptPath := filepath.Join(tmpDir, "debugfs-cmds.txt")
	if err := os.WriteFile(scriptPath, []byte(script+"\n"), 0o644); err != nil {
		return fmt.Errorf("write debugfs script: %w", err)
	}

	cmd := exec.CommandContext(ctx, p.DebugFSPath, "-w", "-f", scriptPath, bootRaw)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("debugfs modify: %w\noutput: %s", err, out)
	}
	log.Info("Boot patcher: debugfs modifications applied", "output", string(out))

	return nil
}

// runDebugFS executes a single debugfs command and returns stdout.
func (p *BootPatcher) runDebugFS(ctx context.Context, devicePath, command string) (string, error) {
	cmd := exec.CommandContext(ctx, p.DebugFSPath, "-R", command, devicePath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w (stderr: %s)", err, stderr.String())
	}
	return stdout.String(), nil
}

// findFilenameByPattern searches debugfs ls output for a filename matching the pattern.
func findFilenameByPattern(lsOutput string, pattern *regexp.Regexp) string {
	match := pattern.FindString(lsOutput)
	return match
}

// PatchGrubCfg modifies a grub.cfg string for VM compatibility:
// GrubPatchOptions controls what PatchGrubCfg modifies in the kernel command line.
type GrubPatchOptions struct {
	// DatasourceOverride is the cloud-init ds= kernel parameter to inject.
	// Examples: "ds=Ec2,none,strict_id:false", "ds=nocloud", "ds=nocloud-net;s=http://..."
	// If empty, no datasource parameter is injected.
	DatasourceOverride string

	// RemapConsole controls whether to remap console=ttyS1 → console=ttyS0.
	// When false, the original console settings are left untouched (use this
	// when the VM has a second serial port capturing ttyS1 directly).
	RemapConsole bool

	// ExtraKernelParams are additional parameters appended to all kernel cmdlines.
	// Example: "ip=dhcp" to configure networking in initrd before systemd starts.
	ExtraKernelParams string
}

// DefaultGrubPatchOptions returns options that force the Ec2 datasource and
// remap console. Use this when NoCloud-net is not available or as a fallback.
func DefaultGrubPatchOptions() GrubPatchOptions {
	return GrubPatchOptions{
		DatasourceOverride: "ds=Ec2,none,strict_id:false",
		RemapConsole:       false,
	}
}

// NoCloudNetGrubPatchOptions returns options that configure the NoCloud-net
// datasource, pointing cloud-init to a per-instance HTTP seed URL.
// This matches the production iPXE flow where qcow-imager injects
// ds=nocloud-net;s=<url> into grub.cfg.
func NoCloudNetGrubPatchOptions(metadataBaseURL, instanceID string) GrubPatchOptions {
	seedURL := fmt.Sprintf("%s/nocloud/%s/", strings.TrimRight(metadataBaseURL, "/"), instanceID)
	// The semicolon must be escaped as \; in grub.cfg because GRUB2 treats ;
	// as a command separator. The backslash is consumed by GRUB's parser,
	// and the literal ; is passed to the kernel cmdline where cloud-init reads it.
	return GrubPatchOptions{
		DatasourceOverride: `ds=nocloud-net\;s=` + seedURL,
		RemapConsole:       false,     // VM has ttyS1 serial port, no remap needed
		ExtraKernelParams:  "ip=dhcp", // configure networking in initrd before systemd to break the luks-tpm ↔ cloud-init ordering cycle
	}
}

// PatchGrubCfg modifies a grub.cfg string to make production images work in QEMU VMs.
//   - Replaces the nvidia kernel filename with the generic one (if oldKernel is non-empty)
//   - Optionally injects a cloud-init datasource override into all kernel cmdlines
//   - Optionally remaps console=ttyS1 → console=ttyS0 for serial visibility
func PatchGrubCfg(grubCfg, oldKernel string, opts GrubPatchOptions) string {
	result := grubCfg

	// Replace kernel filename if specified
	if oldKernel != "" {
		result = strings.ReplaceAll(result, oldKernel, replacementKernelName)
	}

	// Patch kernel cmdlines in all linux boot entries.
	// Match lines like: \tlinux\t/vmlinuz-... root=... <params>
	var patched []string
	for _, line := range strings.Split(result, "\n") {
		trimmed := strings.TrimSpace(line)
		if (strings.HasPrefix(trimmed, "linux\t") || strings.HasPrefix(trimmed, "linux /")) &&
			strings.Contains(trimmed, "root=") {

			// Inject cloud-init datasource override if configured.
			if opts.DatasourceOverride != "" {
				if !strings.Contains(line, " ds=") && !strings.Contains(line, "ci.ds=") {
					line += " " + opts.DatasourceOverride
				}
			}

			// Optionally remap console=ttyS1 → console=ttyS0.
			// Bare-metal uses ttyS1 (COM2), QEMU exposes serial as ttyS0.
			// When the VM has a second serial port, this remap is unnecessary.
			if opts.RemapConsole {
				if strings.Contains(line, "console=ttyS1") {
					line = strings.ReplaceAll(line, "console=ttyS1", "console=ttyS0")
				} else if !strings.Contains(line, "console=ttyS0") {
					line += " console=ttyS0,115200"
				}
			}

			// Append extra kernel parameters (e.g., ip=dhcp).
			if opts.ExtraKernelParams != "" {
				line += " " + opts.ExtraKernelParams
			}
		}
		patched = append(patched, line)
	}
	return strings.Join(patched, "\n")
}
