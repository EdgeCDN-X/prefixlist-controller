package consolidation

import (
	"context"
	"fmt"
	"net"

	edgecdnxv1alpha1 "edgecdnx.com/prefixlist-controller/api/v1alpha1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

func ConsolidateV4(ctx context.Context, prefixes []edgecdnxv1alpha1.V4Prefix) ([]edgecdnxv1alpha1.V4Prefix, error) {
	log := logf.FromContext(ctx)
	log.Info("Consolidating Prefixes")

	for prefixlen := 31; prefixlen > 1; prefixlen-- {
		// Iterating over all prefixes with descending prefix length
		var smallerSubnets []edgecdnxv1alpha1.V4Prefix
		for _, subnet := range prefixes {
			// Find all subnets that have a bit larger size than the current iteration
			// Those potentially can be consolidated
			if subnet.Size-1 == prefixlen {
				smallerSubnets = append(smallerSubnets, subnet)
			}
		}

		if len(smallerSubnets) <= 1 {
			// Not enoughs subnets to consolidate. Skip this iteration
			continue
		}

		consolidables := make(map[string]struct {
			mask     int
			prefixes []net.IP
		})

		for _, subnet := range smallerSubnets {
			// Build a subnet with the current prefix length (subnetB - SubnetBase)
			_, subnetB, err := net.ParseCIDR(subnet.Address + "/" + fmt.Sprint(prefixlen))
			// And also a subnet with the original prefix length (SubnetC - SubnetConsolidable)
			_, subnetC, errC := net.ParseCIDR(subnet.Address + "/" + fmt.Sprint(subnet.Size))
			if err != nil {
				log.Error(err, "Failed to Parse CIDR", "subnet", subnet.Address, "prefixlen", prefixlen)
				continue
			}
			if errC != nil {
				log.Error(errC, "Failed to Parse CIDR", "subnet", subnet.Address, "prefixlen", subnet.Size)
				continue
			}

			if val, ok := consolidables[subnetB.String()]; ok {
				if val.prefixes[0].Equal(subnetC.IP) {
					// If the prefix is already in the map, we can skip it
					continue
				}
				val.prefixes = append(val.prefixes, subnetC.IP)
				consolidables[subnetB.String()] = val
			} else {
				// If the prefix is not in the map, we can add it
				consolidables[subnetB.String()] = struct {
					mask     int
					prefixes []net.IP
				}{
					mask:     subnet.Size,
					prefixes: []net.IP{subnetC.IP},
				}
			}
		}

		for key, val := range consolidables {
			if len(val.prefixes) > 1 {
				log.Info("Consolidating Prefixes", "Target Subnet", key, "Source Subnets", val.prefixes)

				_, newPrefix, err := net.ParseCIDR(key)
				if err != nil {
					log.Error(err, "Failed to Parse CIDR", "subnet", key)
					continue
				}

				ones, _ := newPrefix.Mask.Size()

				prefixes = append(prefixes, edgecdnxv1alpha1.V4Prefix{
					Address: newPrefix.IP.String(),
					Size:    ones,
				})
			}
		}
	}

	log.Info("Consolidated Prefixes", "Result", prefixes)
	log.Info("Finding supernets")

	toBeDeleted := make(map[string]edgecdnxv1alpha1.V4Prefix, 0)

	for prefixlen := range 32 {
		for _, subnet := range prefixes {
			if subnet.Size == prefixlen {
				// For each prefix we are looging for a supernet

				_, subnetS, err := net.ParseCIDR(subnet.Address + "/" + fmt.Sprint(prefixlen))
				if err != nil {
					log.Error(err, "Failed to Parse CIDR", "subnet", subnet.Address, "prefixlen", prefixlen)
					continue
				}

				for _, subnetCh := range prefixes {
					if prefixlen < subnetCh.Size {
						_, subnetChP, err := net.ParseCIDR(subnetCh.Address + "/" + fmt.Sprint(subnetCh.Size))
						if err != nil {
							log.Error(err, "Failed to Parse CIDR", "subnet", subnetCh.Address, "prefixlen", subnetCh.Size)
							continue
						}
						if subnetS.Contains(subnetChP.IP) {
							// We mark this entry for deletion
							log.Info("Subnet is a supernet. Marking subnet for delete", "Subnet", subnetChP.String(), "Supernet", subnetS.String())
							_, ok := toBeDeleted[subnetChP.String()]
							if ok {
								continue
							} else {
								toBeDeleted[subnetChP.String()] = subnetCh
							}
						}
					}
				}

			}
		}
	}

	keepPrefixes := make([]edgecdnxv1alpha1.V4Prefix, 0)
	for _, prefix := range prefixes {
		_, ok := toBeDeleted[fmt.Sprintf("%s/%d", prefix.Address, prefix.Size)]
		if !ok {
			keepPrefixes = append(keepPrefixes, prefix)
		}
	}
	log.Info("Consolidated Prefixes", "Result", keepPrefixes)

	return keepPrefixes, nil
}
