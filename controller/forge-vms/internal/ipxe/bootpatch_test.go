// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ipxe

import (
	"encoding/binary"
	"runtime"
	"testing"
)

func TestBootNeedsKernelSwap(t *testing.T) {
	got := NeedsKernelSwap()
	want := runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
	if got != want {
		t.Errorf("NeedsKernelSwap() = %v, want %v (GOOS=%s, GOARCH=%s)",
			got, want, runtime.GOOS, runtime.GOARCH)
	}
}

func TestBootPatchGrubCfg(t *testing.T) {
	tests := []struct {
		name      string
		grubCfg   string
		oldKernel string
		opts      GrubPatchOptions
		want      string
	}{
		{
			name: "kernel replacement plus ds injection (no console remap)",
			grubCfg: `menuentry 'Ubuntu' {
	linux	/vmlinuz-6.8.0-1047-nvidia-64k root=/dev/mapper/ubuntu--vg-ubuntu--lv ro console=ttyAMA0,115200
	initrd	/initrd.img-6.8.0-1047-nvidia-64k
}`,
			oldKernel: "vmlinuz-6.8.0-1047-nvidia-64k",
			opts:      DefaultGrubPatchOptions(),
			want: `menuentry 'Ubuntu' {
	linux	/vmlinuz-generic root=/dev/mapper/ubuntu--vg-ubuntu--lv ro console=ttyAMA0,115200 ds=Ec2,none,strict_id:false
	initrd	/initrd.img-6.8.0-1047-nvidia-64k
}`,
		},
		{
			name: "ds and console remap (ttyS1→ttyS0) with explicit remap option",
			grubCfg: `linux /vmlinuz-6.8.0-1047-nvidia root=/dev/mapper/ubuntu--vg-ubuntu--lv ro console=ttyS1,115200 luks.key=changeit
initrd /initrd.img-6.8.0-1047-nvidia`,
			opts: GrubPatchOptions{DatasourceOverride: "ds=Ec2,none,strict_id:false", RemapConsole: true},
			want: `linux /vmlinuz-6.8.0-1047-nvidia root=/dev/mapper/ubuntu--vg-ubuntu--lv ro console=ttyS0,115200 luks.key=changeit ds=Ec2,none,strict_id:false
initrd /initrd.img-6.8.0-1047-nvidia`,
		},
		{
			name:    "already has ci.ds=nocloud and console=ttyS0",
			grubCfg: "linux /vmlinuz root=/dev/sda1 ci.ds=nocloud console=ttyS0,115200",
			opts:    DefaultGrubPatchOptions(),
			want:    "linux /vmlinuz root=/dev/sda1 ci.ds=nocloud console=ttyS0,115200",
		},
		{
			name:    "non-linux lines unchanged",
			grubCfg: "set default=0\ninsmod gzio",
			opts:    DefaultGrubPatchOptions(),
			want:    "set default=0\ninsmod gzio",
		},
		{
			name: "ds only, no console remap (second serial port captures ttyS1)",
			grubCfg: `linux /vmlinuz root=/dev/mapper/ubuntu--vg-ubuntu--lv ro console=ttyS1,115200 luks.key=changeit
initrd /initrd.img`,
			opts: GrubPatchOptions{DatasourceOverride: "ds=Ec2,none,strict_id:false", RemapConsole: false},
			want: `linux /vmlinuz root=/dev/mapper/ubuntu--vg-ubuntu--lv ro console=ttyS1,115200 luks.key=changeit ds=Ec2,none,strict_id:false
initrd /initrd.img`,
		},
		{
			name:    "no patching at all",
			grubCfg: "linux /vmlinuz root=/dev/sda1 console=ttyS1,115200",
			opts:    GrubPatchOptions{},
			want:    "linux /vmlinuz root=/dev/sda1 console=ttyS1,115200",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PatchGrubCfg(tt.grubCfg, tt.oldKernel, tt.opts)
			if got != tt.want {
				t.Errorf("PatchGrubCfg() =\n%s\n\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestBootFindFilenameByPattern(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   string
		isInit bool
	}{
		{
			name:  "finds nvidia-64k kernel",
			input: " 131073  40700  2   2   4096 23-Oct-2024 08:15 .\n 393218 100644  0   0  14832640 23-Oct-2024 08:14 vmlinuz-6.8.0-1047-nvidia-64k\n",
			want:  "vmlinuz-6.8.0-1047-nvidia-64k",
		},
		{
			name:   "finds nvidia-64k initrd",
			input:  " 393219 100644  0   0  98765432 23-Oct-2024 08:14 initrd.img-6.8.0-1047-nvidia-64k\n",
			want:   "initrd.img-6.8.0-1047-nvidia-64k",
			isInit: true,
		},
		{
			name:  "no match returns empty",
			input: " 393218 100644  0   0  14832640 23-Oct-2024 08:14 vmlinuz-5.15.0-generic\n",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pat := nvidiaKernelPattern
			if tt.isInit {
				pat = nvidiaInitrdPattern
			}
			got := findFilenameByPattern(tt.input, pat)
			if got != tt.want {
				t.Errorf("findFilenameByPattern() = %q, want %q", got, tt.want)
			}
		})
	}
}

// buildMockGPT creates a minimal GPT (protective MBR + header + 3 partition entries)
// for testing the partition parser.
func buildMockGPT(partitions []gptPartitionEntry) []byte {
	// Allocate 34 sectors (MBR + GPT header + 32 sectors of entries).
	data := make([]byte, 34*sectorSize)

	// Write GPT header at LBA 1.
	hdr := data[sectorSize:]
	copy(hdr[0:8], []byte(gptSignature))

	// Revision 1.0
	binary.LittleEndian.PutUint32(hdr[8:12], 0x00010000)
	// Header size
	binary.LittleEndian.PutUint32(hdr[12:16], 92)
	// Partition entry start LBA = 2
	binary.LittleEndian.PutUint64(hdr[72:80], 2)
	// Number of partition entries
	binary.LittleEndian.PutUint32(hdr[80:84], uint32(len(partitions)))
	// Size of each entry = 128
	binary.LittleEndian.PutUint32(hdr[84:88], 128)

	// Write partition entries starting at LBA 2.
	entryBase := 2 * sectorSize
	for i, p := range partitions {
		off := entryBase + i*128
		copy(data[off:off+16], p.TypeGUID[:])
		// Unique GUID at offset 16..32 — leave zeroed for tests.
		binary.LittleEndian.PutUint64(data[off+32:off+40], p.StartLBA)
		binary.LittleEndian.PutUint64(data[off+40:off+48], p.EndLBA)
		// Attributes at 48..56 — leave zeroed.
		// Name at 56..128 — UTF-16LE.
		nameBytes := encodeUTF16LE(p.Name)
		nameFieldLen := 128 - 56
		if len(nameBytes) > nameFieldLen {
			nameBytes = nameBytes[:nameFieldLen]
		}
		copy(data[off+56:off+56+len(nameBytes)], nameBytes)
	}

	return data
}

func TestBootFindBootPartition(t *testing.T) {
	// Non-zero type GUID to mark partitions as non-empty.
	efiGUID := [16]byte{0x28, 0x73, 0x2a, 0xc1, 0x1f, 0xf8, 0xd2, 0x11, 0xba, 0x4b, 0x00, 0xa0, 0xc9, 0x3e, 0xc9, 0x3b}
	linuxGUID := [16]byte{0xaf, 0x3d, 0xc6, 0x0f, 0x83, 0x84, 0x72, 0x47, 0x8e, 0x79, 0x3d, 0x69, 0xd8, 0x47, 0x7d, 0xe4}

	tests := []struct {
		name       string
		partitions []gptPartitionEntry
		wantStart  uint64
		wantSec    uint64
		wantErr    bool
	}{
		{
			name: "standard 3-partition layout",
			partitions: []gptPartitionEntry{
				{TypeGUID: efiGUID, StartLBA: 2048, EndLBA: 1050623, Name: "EFI System Partition"},
				{TypeGUID: linuxGUID, StartLBA: 1050624, EndLBA: 3147775, Name: "boot"},
				{TypeGUID: linuxGUID, StartLBA: 3147776, EndLBA: 104857599, Name: "root"},
			},
			wantStart: 1050624,
			wantSec:   3147775 - 1050624 + 1,
		},
		{
			name: "only one partition",
			partitions: []gptPartitionEntry{
				{TypeGUID: efiGUID, StartLBA: 2048, EndLBA: 1050623, Name: "EFI"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gptData := buildMockGPT(tt.partitions)
			start, sec, err := findBootPartition(gptData)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if start != tt.wantStart {
				t.Errorf("startLBA = %d, want %d", start, tt.wantStart)
			}
			if sec != tt.wantSec {
				t.Errorf("sectors = %d, want %d", sec, tt.wantSec)
			}
		})
	}
}

func TestBootParseUTF16LEName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple ASCII", "boot", "boot"},
		{"EFI partition", "EFI System Partition", "EFI System Partition"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := encodeUTF16LE(tt.input)
			got := parseUTF16LEName(encoded)
			if got != tt.want {
				t.Errorf("parseUTF16LEName(encodeUTF16LE(%q)) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBootGPTDataTooShort(t *testing.T) {
	// Ensure findBootPartition returns an error for truncated data.
	_, _, err := findBootPartition([]byte("short"))
	if err == nil {
		t.Fatal("expected error for short GPT data")
	}
}

func TestBootGPTInvalidSignature(t *testing.T) {
	data := make([]byte, 34*sectorSize)
	copy(data[sectorSize:sectorSize+8], []byte("NOT GPT!"))
	_, _, err := findBootPartition(data)
	if err == nil {
		t.Fatal("expected error for invalid GPT signature")
	}
}

func TestBootGPTEmptyPartition2(t *testing.T) {
	// Non-zero type GUID for partition 1, but partition 2 is all zeros.
	efiGUID := [16]byte{0x28, 0x73, 0x2a, 0xc1}
	gptData := buildMockGPT([]gptPartitionEntry{
		{TypeGUID: efiGUID, StartLBA: 2048, EndLBA: 4095, Name: "EFI"},
		{TypeGUID: [16]byte{}, StartLBA: 0, EndLBA: 0, Name: ""},
	})
	// The second entry has all-zero type GUID so it will be skipped,
	// resulting in fewer than 2 entries.
	_, _, err := findBootPartition(gptData)
	if err == nil {
		t.Fatal("expected error for empty partition 2")
	}
}
