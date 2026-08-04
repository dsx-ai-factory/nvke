package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	pb "github.com/NVIDIA/nke/node-agent/pkg/agent/proto"
	nkeutils "github.com/NVIDIA/nke/node-agent/pkg/utils"
)

type DiskEncryptionStatus struct {
	EncryptedDeviceUUID    string   `json:"encryptedDeviceUUID"`
	KeylessEntryConfig     string   `json:"keylessEntryConfig"`
	KeylessEntryEnabled    bool     `json:"keylessEntryEnabled"`
	KeylessEntryConfigured bool     `json:"keylessEntryConfigured"`
	TPMDecryptionEnabled   bool     `json:"tpmDecryptionEnabled"`
	MasterKeyEnabled       bool     `json:"masterKeyEnabled"`
	ConfiguredPCRIds       []string `json:"configuredTPMKeys"`
}

func (s *NodeAgentServer) ConfigureTPMUnlockForFDE(
	ctx context.Context,
	req *pb.ConfigureTPMUnlockForFDERequest,
) (*pb.ConfigureTPMUnlockForFDEResponse, error) {
	resp := &pb.ConfigureTPMUnlockForFDEResponse{}

	defer logRPCSpan(ctx, "ConfigureTPMUnlockForFDE")()
	defer s.PersistNodeStatus() //nolint:all

	// First off, make sure it hasn't already been configured...
	if s.NodeStatus.DiskEncryptionStatus.TPMDecryptionEnabled {
		log.Printf("TPM decryption already enabled")
		resp.ErrorMessage = "TPM decryption already enabled"
		return resp, errors.New("TPM decryption already enabled")
	}

	// First we need to find the encrypted root device
	// NOTE: This is not an exhaustive search, we are assuming the root device is a partition and
	// that encryption was enabled during the OS installation using LVM and LUKS
	// We also make assumptions about our OS (BaseOS which is a derivative of Ubuntu)

	// NOTE: We now have the UUID from keyless-entry config and can simplify this, BUT for now we're going to leave
	// the search routine as is, perhaps revisit in the future and simplify, but for now the routine works so leave it

	var outputOverride []string // We use the default outputs, just pass empty slice
	blkDeviceInfo, err := s.getLSBLKInfo(outputOverride)
	if err != nil {
		resp.ErrorMessage = fmt.Sprintf("error getting block device info: %s", err.Error())
		return resp, err
	}

	dev, err := findEncryptedRootDevice(blkDeviceInfo)
	if err != nil {
		resp.ErrorMessage = fmt.Sprintf("error finding encrypted root device:%s", err.Error())
		return resp, err
	}

	device := "/dev/" + dev

	if err := s.configureTPMUnlock(req.CurrentMasterKey, device, req.Pcrs); err != nil {
		log.Printf("failed to configure TPM unlock: %v\n", err)
		resp.ErrorMessage = fmt.Sprintf("error configuring TPM unlock: %s", err.Error())
		return resp, err
	}
	s.NodeStatus.DiskEncryptionStatus.TPMDecryptionEnabled = true
	s.NodeStatus.DiskEncryptionStatus.ConfiguredPCRIds = req.Pcrs

	// keyless entry was used simply to bootstrap the node, once the node is online we expect
	// that production nodes will be hardened (via this call) to remove keyless entry and
	// set secure keys for unlocking the LUKS device
	if err = s.removeKeylessEntry(); err != nil {
		log.Printf("failed to remove keyless entry: %v\n", err)
		resp.ErrorMessage = fmt.Sprintf("error removing keyless entry: %s", err.Error())
		return resp, err
	}

	// We ALWAYS need a master key!!!  If you don't have a master key not only can you
	// never recover if there's an issue, but more importantly you can never update the
	// clevis settings (e.g. add/remove PCRIds) without a master key
	// This also means that at some point if we go the next step and utilize PCRs that are
	// modified by kernel updates, we will need to have a mechanism to update/rebind to the changed PCR values
	err = s.addMasterKeyToLuks(req.NewMasterKey, req.CurrentMasterKey, device)
	if err != nil {
		resp.ErrorMessage = fmt.Sprintf("error adding master key to LUKS: %s", err.Error())
		return resp, err
	}
	s.NodeStatus.DiskEncryptionStatus.MasterKeyEnabled = true

	// Finally, remove our initial bootstrap key from the LUKS key store
	err = s.removeKeyFromLuks(device, req.CurrentMasterKey)
	if err != nil {
		resp.ErrorMessage = fmt.Sprintf("error removing key from LUKS: %s", err.Error())
		return resp, err
	}

	return resp, nil
}

type LSBLK struct {
	Blockdevices []BlockDevice `json:"blockdevices"`
}

type BlockDevice struct {
	Name        string        `json:"name"`        // device name
	Type        string        `json:"type"`        // device type e.g. loop, disk, part, crypt, lvm
	Mountpoints []string      `json:"mountpoints"` // path where the device is mounted
	UUID        string        `json:"uuid"`        // UUID of the device
	Children    []BlockDevice `json:"children"`    // list of children devices
}

func childIncludesRootMountpoint(children []BlockDevice) bool {
	for _, child := range children {
		log.Printf("looking for root mountpoint on: %s\n", child.Name)
		for _, mountpoint := range child.Mountpoints {
			if mountpoint == "/" {
				return true
			}
		}
	}
	return false
}

func childIncludesCryptWithRootChild(children []BlockDevice) bool {
	for _, child := range children {
		log.Printf("looking for crypt with root child on: %s...\n", child.Name)
		if child.Type == "crypt" {
			if childIncludesRootMountpoint(child.Children) {
				return true
			}
		}
	}
	return false
}

func hasPartitionWithChildren(bds []BlockDevice) bool {
	log.Printf("searching for partition with children...\n")
	for _, bd := range bds {
		if bd.Type == "part" && len(bd.Children) > 0 {
			return true
		}
	}
	return false
}

func (s *NodeAgentServer) getLSBLKInfo(outputsOverride []string) (string, error) {
	params := []string{"--json", "-o", "name", "type", "mountpoints", "uuid"}
	if len(outputsOverride) > 0 {
		params = outputsOverride
	}
	out, err := s.Runner.Run("lsblk", params, &nkeutils.CommandOptions{})
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

func findEncryptedRootDevice(blkInfo string) (string, error) {
	var lsblk LSBLK
	err := json.Unmarshal([]byte(blkInfo), &lsblk)
	if err != nil {
		log.Printf("failed to unmarshal lsblk output: %v\n", err)
		return "", err
	}

	log.Printf("searching for encrypted root partition...\n")
	for _, bd := range lsblk.Blockdevices {
		if bd.Type == "disk" && hasPartitionWithChildren(bd.Children) {
			for _, child := range bd.Children {
				if childIncludesCryptWithRootChild(child.Children) {
					log.Printf("found encrypted root partition: %s\n", child.Name)
					return child.Name, nil
				}
			}
		}
	}
	return "", errors.New("unable to find encrypted root device")
}

func (s *NodeAgentServer) removeKeylessEntry() error {
	log.Println("issue keyless-entry disable...")
	output, err := s.Runner.Run("keyless-entry.sh", []string{"disable"}, &nkeutils.CommandOptions{})
	if err != nil {
		log.Printf("failed to disable keyless entry: %s\n", string(output))
		return fmt.Errorf("failed to disable keyless entry: %s", string(output))
	}

	s.NodeStatus.DiskEncryptionStatus.KeylessEntryEnabled = false

	log.Println("issue keyless-entry unconfigure...")
	output, err = s.Runner.Run("keyless-entry.sh", []string{"unconfigure"}, &nkeutils.CommandOptions{})
	if err != nil {
		log.Printf("failed to unconfigure keyless entry: %s\n", string(output))
		return fmt.Errorf("failed to unconfigure keyless entry: %s", string(output))
	}

	s.NodeStatus.DiskEncryptionStatus.KeylessEntryConfigured = false
	return nil
}

func (s *NodeAgentServer) addMasterKeyToLuks(newKey, luksPassword, diskPath string) error {
	input := fmt.Sprintf("%s\n%s\n%s\n", luksPassword, newKey, newKey)
	opts := &nkeutils.CommandOptions{
		Stdin: bytes.NewBufferString(input),
	}

	out, err := s.Runner.Run("cryptsetup", []string{"luksAddKey", diskPath}, opts)
	if err != nil {
		log.Printf("error executing luksAddKey: %v\n", err)
		log.Printf("stderr/stdout: %s\n", string(out))
		return err
	}

	log.Printf("stdout from luksAddKey: %s\n", string(out))
	return nil

	/* Alternate impl with bash call and runner..
	cmdStr := fmt.Sprintf("cryptsetup luksAddKey %s", diskPath)

	// Prepare input for the prompts
	input := fmt.Sprintf("%s\n%s\n%s\n", luksPassword, newKey, newKey)
	opts := &nkeutils.CommandOptions{
		Stdin: bytes.NewBufferString(input),
	}

	// Run command via bash -c, since it's still a shell string
	out, err := runner.Run("/bin/bash", []string{"-c", cmdStr}, opts)
	if err != nil {
		log.Printf("error executing luksAddKey: %v\n", err)
		log.Printf("stderr/stdout: %s\n", string(out))
		return err
	}

	log.Printf("stdout from luksAddKey: %s\n", string(out))
	return nil
	*/

	/* old impl with bash call...
	  cmdStr := fmt.Sprintf("cryptsetup luksAddKey %s", diskPath)

		// Prepare input for the prompts
		input := fmt.Sprintf("%s\n%s\n%s\n", luksPassword, newKey, newKey)
		opts := &nkeutils.CommandOptions{
			Stdin: bytes.NewBufferString(input),
		}

		// Run command via bash -c, since it's still a shell string
		out, err := runner.Run("/bin/bash", []string{"-c", cmdStr}, opts)
		if err != nil {
			log.Printf("error executing luksAddKey: %v\n", err)
			log.Printf("stderr/stdout: %s\n", string(out))
			return err
		}

		log.Printf("stdout from luksAddKey: %s\n", string(out))
		return nil
	*/
}

func (s *NodeAgentServer) removeKeyFromLuks(diskPath, keyToRemove string) error {
	// RemoveKey is different, you don't need to unlock to
	// remove a key.
	// Makes sense because you already "know" the key otherwise you couldn't remove it
	// 1. "Enter passphrase to be deleted:"
	input := fmt.Sprintf("%s\n", keyToRemove)

	var out bytes.Buffer
	var stderr bytes.Buffer

	// Run the command
	log.Println("issue cryptsetup luksRemoveKey...")

	cmdStr := fmt.Sprintf("cryptsetup luksRemoveKey %s", diskPath)
	_, err := s.Runner.Run("/bin/bash", []string{"-c", cmdStr}, &nkeutils.CommandOptions{
		Stdin:  bytes.NewBufferString(input),
		Stdout: &out,
		Stderr: &stderr,
	})
	if err != nil {
		log.Printf("error executing luksRemoveKey: %v\n", err)
		log.Printf("stderr: %s\n", stderr.String())
		return err
	}
	log.Printf("stdout from luksRemoveKey: %s\n", out.String())
	return nil
}

func (s *NodeAgentServer) configureTPMUnlock(luksPassword, diskPath string, pcrIds []string) error {
	// We limit the PCRIds for now to 1, 4, 5, 7
	// These will work fine for most TPM 2.0 devices and will not require rebuild or reconifguration
	// on kernel changes.  If TPMS that are modified by kernel modules or update are added to the PcrIds
	// we will require a rebuild of the initramfs and rebind via clevis
	// In the future we can add ability to specify these, but we MUST be careful if ids that change
	// with kernel modifications are added (add/remove module, update version etc) we don't
	// have a mechanism to ensure we rebuild initramsf and rebind the TPM in clevis

	// Full clevis command with bash -c (clevis doesn't like to be called directly from exec.Command)
	// cmdStr := `clevis luks bind -d /dev/nvme2n1p3 tpm2 '{"pcr_bank": "sha256", "pcr_ids": "1,5,7,9"}'`

	var out bytes.Buffer
	var stderr bytes.Buffer

	cmdStr := fmt.Sprintf(
		`clevis luks bind -d %s tpm2 '{"pcr_bank": "sha256", "pcr_ids": "%s"}'`,
		diskPath,
		pcrIds,
	)

	log.Printf("issue clevis tpm setup; cmd: %v\n", cmdStr)
	_, err := s.Runner.Run("/bin/bash", []string{"-c", cmdStr}, &nkeutils.CommandOptions{
		Stdin:  bytes.NewBufferString(luksPassword),
		Stdout: &out,
		Stderr: &stderr,
	})
	if err != nil {
		log.Printf("error configuring TPM unlock: %v\n", err)
		log.Printf("stderr: %s\n", stderr.String())
		return err
	}
	log.Printf("stdout: %s\n", out.String())
	return nil
}
