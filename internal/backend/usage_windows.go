package backend

import "golang.org/x/sys/windows"

func diskUsage(dir string) (Usage, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return Usage{}, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return Usage{}, err
	}
	return Usage{Total: total, Free: free}, nil
}
