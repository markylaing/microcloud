package main

import (
	"fmt"
	"sort"
	"strings"

	cephTypes "github.com/canonical/microceph/microceph/api/types"
	"github.com/lxc/lxd/lxc/utils"
	lxdAPI "github.com/lxc/lxd/shared/api"
	cli "github.com/lxc/lxd/shared/cmd"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/units"

	"github.com/canonical/microcloud/microcloud/api/types"
	"github.com/canonical/microcloud/microcloud/mdns"
	"github.com/canonical/microcloud/microcloud/service"
)

// askRetry will print all errors and re-attempt the given function on user input.
func askRetry(question string, autoSetup bool, f func() error) {
	for {
		retry := false
		err := f()
		if err != nil {
			fmt.Println(err)

			if !autoSetup {
				retry, err = cli.AskBool(fmt.Sprintf("%s (yes/no) [default=yes]: ", question), "yes")
				if err != nil {
					fmt.Println(err)
					retry = false
				}
			}
		}

		if !retry {
			break
		}
	}
}

func askMissingServices(services []types.ServiceType, stateDirs map[types.ServiceType]string, autoSetup bool) ([]types.ServiceType, error) {
	missingServices := []string{}
	for serviceType, stateDir := range stateDirs {
		if service.ServiceExists(serviceType, stateDir) {
			services = append(services, serviceType)
		} else {
			missingServices = append(missingServices, string(serviceType))
		}
	}

	if len(missingServices) > 0 {
		serviceStr := strings.Join(missingServices, ",")
		if !autoSetup {
			skip, err := cli.AskBool(fmt.Sprintf("%s not found. Continue anyway? (yes/no) [default=yes]: ", serviceStr), "yes")
			if err != nil {
				return nil, err
			}

			if !skip {
				return services, nil
			}
		}

		logger.Infof("Skipping %s (could not detect service state directory)", serviceStr)
	}

	return services, nil
}

func askDisks(sh *service.ServiceHandler, peers map[string]mdns.ServerInfo, bootstrap bool, autoSetup bool, wipeAllDisks bool) (map[string][]lxdAPI.ClusterMemberConfigKey, map[string][]cephTypes.DisksPost, error) {
	if bootstrap {
		// Add the local system to the list of peers so we can select disks.
		peers[sh.Name] = mdns.ServerInfo{Name: sh.Name}
		defer delete(peers, sh.Name)
	}

	allResources := make(map[string]*lxdAPI.Resources, len(peers))
	var err error
	for peer, info := range peers {
		allResources[peer], err = sh.Services[types.LXD].(*service.LXDService).GetResources(sh.Name != peer, peer, info.Address, info.AuthSecret)
		if err != nil {
			return nil, nil, fmt.Errorf("Failed to get system resources of peer %q: %w", peer, err)
		}
	}

	validDisks := make(map[string][]lxdAPI.ResourcesStorageDisk, len(allResources))
	for peer, r := range allResources {
		validDisks[peer] = make([]lxdAPI.ResourcesStorageDisk, 0, len(r.Storage.Disks))
		for _, disk := range r.Storage.Disks {
			if len(disk.Partitions) == 0 {
				validDisks[peer] = append(validDisks[peer], disk)
			}
		}
	}

	var diskConfig map[string][]lxdAPI.ClusterMemberConfigKey
	var reservedDisks map[string]string
	wantsDisks := true
	if !autoSetup {
		wantsDisks, err = cli.AskBool("Would you like to setup local storage? (yes/no) [default=yes]: ", "yes")
		if err != nil {
			return nil, nil, err
		}
	}

	lxd := sh.Services[types.LXD].(*service.LXDService)
	if wantsDisks {
		askRetry("Retry selecting disks?", autoSetup, func() error {
			diskConfig, reservedDisks, err = askLocalPool(validDisks, autoSetup, wipeAllDisks, *lxd)

			return err
		})
	}

	var cephDisks map[string][]cephTypes.DisksPost
	if sh.Services[types.MicroCeph] != nil {
		ceph := sh.Services[types.MicroCeph].(*service.CephService)
		wantsDisks = true
		if !autoSetup {
			wantsDisks, err = cli.AskBool("Would you like to setup distributed storage? (yes/no) [default=yes]: ", "yes")
			if err != nil {
				return nil, nil, err
			}
		}

		if wantsDisks {
			askRetry("Retry selecting disks?", autoSetup, func() error {
				cephDisks, err = askRemotePool(validDisks, reservedDisks, autoSetup, wipeAllDisks, *ceph)

				return err
			})
		}
	}

	if !bootstrap {
		sourceTemplate := lxdAPI.ClusterMemberConfigKey{
			Entity: "storage-pool",
			Name:   "remote",
			Key:    "source",
			Value:  "lxd_remote",
		}

		for peer := range cephDisks {
			diskConfig[peer] = append(diskConfig[peer], sourceTemplate)
		}
	}

	return diskConfig, cephDisks, nil
}

func askLocalPool(peerDisks map[string][]lxdAPI.ResourcesStorageDisk, autoSetup bool, wipeAllDisks bool, lxd service.LXDService) (map[string][]lxdAPI.ClusterMemberConfigKey, map[string]string, error) {
	data := [][]string{}
	selected := map[string]string{}
	for peer, disks := range peerDisks {
		// If there's no spare disk, then we can't add a remote storage pool, so skip local pool creation.
		if autoSetup && len(disks) < 2 {
			logger.Infof("Skipping local storage pool creation, peer %q has too few disks", peer)

			return nil, nil, nil
		}

		for _, disk := range disks {
			devicePath := fmt.Sprintf("/dev/disk/by-id/%s", disk.DeviceID)
			data = append(data, []string{peer, disk.Model, units.GetByteSizeStringIEC(int64(disk.Size), 2), disk.Type, devicePath})

			// Add the first disk for each peer.
			if autoSetup {
				_, ok := selected[peer]
				if !ok {
					selected[peer] = devicePath
				}
			}
		}
	}

	toWipe := map[string]string{}
	wipeable, err := lxd.HasExtension(false, lxd.Name(), lxd.Address(), "", "storage_pool_source_wipe")
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to check for source.wipe extension: %w", err)
	}

	if !autoSetup {
		sort.Sort(utils.ByName(data))
		header := []string{"LOCATION", "MODEL", "CAPACITY", "TYPE", "PATH"}
		table := NewSelectableTable(header, data)

		// map the rows (as strings) to the associated row.
		rowMap := make(map[string][]string, len(data))
		for i, r := range table.rows {
			rowMap[r] = data[i]
		}

		fmt.Println("Select exactly one disk from each cluster member:")
		selectedRows, err := table.Render(table.rows)
		if err != nil {
			return nil, nil, fmt.Errorf("Failed to confirm local LXD disk selection: %w", err)
		}

		for _, entry := range selectedRows {
			target := rowMap[entry][0]
			path := rowMap[entry][4]

			_, ok := selected[target]
			if ok {
				return nil, nil, fmt.Errorf("Failed to add local storage pool: Selected more than one disk for target peer %q", target)
			}

			selected[target] = path
		}

		if !wipeAllDisks && wipeable {
			fmt.Println("Select which disks to wipe:")
			wipeRows, err := table.Render(selectedRows)
			if err != nil {
				return nil, nil, fmt.Errorf("Failed to confirm which disks to wipe: %w", err)
			}

			for _, entry := range wipeRows {
				target := rowMap[entry][0]
				path := rowMap[entry][4]
				toWipe[target] = path
			}
		}
	}

	if len(selected) == 0 {
		return nil, nil, nil
	}

	if len(selected) != len(peerDisks) {
		return nil, nil, fmt.Errorf("Failed to add local storage pool: Some peers don't have an available disk")
	}

	if wipeAllDisks && wipeable {
		toWipe = selected
	}

	wipeDisk := lxdAPI.ClusterMemberConfigKey{
		Entity: "storage-pool",
		Name:   "local",
		Key:    "source.wipe",
		Value:  "true",
	}

	sourceTemplate := lxdAPI.ClusterMemberConfigKey{
		Entity: "storage-pool",
		Name:   "local",
		Key:    "source",
	}

	memberConfig := make(map[string][]lxdAPI.ClusterMemberConfigKey, len(selected))
	for target, path := range selected {
		if target == lxd.Name() {
			err := lxd.AddLocalPool(path, wipeable && toWipe[target] != "")
			if err != nil {
				return nil, nil, fmt.Errorf("Failed to add pending local storage pool on peer %q: %w", target, err)
			}
		} else {
			sourceTemplate.Value = path
			memberConfig[target] = []lxdAPI.ClusterMemberConfigKey{sourceTemplate}
			if toWipe[target] != "" {
				memberConfig[target] = append(memberConfig[target], wipeDisk)
			}
		}
	}

	return memberConfig, selected, nil
}

func askRemotePool(peerDisks map[string][]lxdAPI.ResourcesStorageDisk, localDisks map[string]string, autoSetup bool, wipeAllDisks bool, ceph service.CephService) (map[string][]cephTypes.DisksPost, error) {
	header := []string{"LOCATION", "MODEL", "CAPACITY", "TYPE", "PATH"}
	data := [][]string{}
	for peer, disks := range peerDisks {
		for _, disk := range disks {
			// Skip any disks that have been reserved for the local storage pool.
			devicePath := fmt.Sprintf("/dev/disk/by-id/%s", disk.DeviceID)
			if localDisks != nil && localDisks[peer] == devicePath {
				continue
			}

			data = append(data, []string{peer, disk.Model, units.GetByteSizeStringIEC(int64(disk.Size), 2), disk.Type, devicePath})
		}
	}

	if len(data) == 0 {
		return nil, fmt.Errorf("Found no available disks")
	}

	sort.Sort(utils.ByName(data))
	table := NewSelectableTable(header, data)
	selected := table.rows
	var toWipe []string
	if wipeAllDisks {
		toWipe = selected
	}

	// map the rows (as strings) to the associated row.
	rowMap := make(map[string][]string, len(data))
	for i, r := range table.rows {
		rowMap[r] = data[i]
	}

	if len(table.rows) == 0 {
		return nil, nil
	}

	if !autoSetup {
		fmt.Println("Select from the available unpartitioned disks:")
		var err error
		selected, err = table.Render(table.rows)
		if err != nil {
			return nil, fmt.Errorf("Failed to confirm disk selection: %w", err)
		}

		if len(selected) > 0 && !wipeAllDisks {
			fmt.Println("Select which disks to wipe:")
			toWipe, err = table.Render(selected)
			if err != nil {
				return nil, fmt.Errorf("Failed to confirm disk wipe selection: %w", err)
			}
		}
	}

	wipeMap := make(map[string]bool, len(toWipe))
	for _, entry := range toWipe {
		_, ok := rowMap[entry]
		if ok {
			wipeMap[entry] = true
		}
	}

	diskMap := map[string][]cephTypes.DisksPost{}
	for _, entry := range selected {
		target := rowMap[entry][0]
		path := rowMap[entry][4]

		_, ok := diskMap[target]
		if !ok {
			diskMap[target] = []cephTypes.DisksPost{}
		}

		diskMap[target] = append(diskMap[target], cephTypes.DisksPost{Path: path, Wipe: wipeMap[entry]})
	}

	_, checkMinSize := peerDisks[ceph.Name()]
	if len(diskMap) == len(peerDisks) {
		if !checkMinSize || len(peerDisks) >= 3 {
			return diskMap, nil
		}
	}

	return nil, fmt.Errorf("Unable to add remote storage pool: Each peer (minimum 3) must have allocated disks")
}
