//go:build !linux && !darwin

package sandbox

// apply restricts nothing: no portable mechanism exists on this platform.
func apply(spec Spec, _ string) ([]string, error) {
	if !spec.RestrictsFS() && !spec.DenyNet && spec.AddressSpace == 0 && spec.FileSize == 0 &&
		spec.OpenFiles == 0 && spec.Processes == 0 && spec.ProcessLimit == 0 && spec.Nice == 0 {
		return nil, nil
	}
	return []string{"sandbox restrictions are not available on this platform"}, nil
}
