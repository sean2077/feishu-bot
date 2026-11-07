package process

import (
	"context"
	"errors"
)

// ResidentTreeBytes observes this process and its currently parented descendants.
// It is an admission signal, not an OS memory limit: RSS includes shared pages,
// existing work can grow, and a process snapshot is not an atomic tree census.
func ResidentTreeBytes(ctx context.Context) (int64, error) {
	rows, root, err := residentRows(ctx)
	if err != nil {
		return 0, err
	}
	return sumResidentTree(rows, root)
}

type residentProcess struct {
	pid, parent int
	bytes       int64
}

func residentDescendants(rows []residentProcess, root int) map[int]bool {
	included := map[int]bool{root: true}
	for {
		before := len(included)
		for _, row := range rows {
			if included[row.parent] {
				included[row.pid] = true
			}
		}
		if before == len(included) {
			return included
		}
	}
}

func sumResidentTree(rows []residentProcess, root int) (int64, error) {
	included := residentDescendants(rows, root)
	var total int64
	found := false
	for _, row := range rows {
		if !included[row.pid] {
			continue
		}
		if row.bytes < 0 || row.bytes > (1<<63-1)-total {
			return 0, errors.New("process tree RSS sample is incomplete")
		}
		total += row.bytes
		found = found || row.pid == root
	}
	if !found {
		return 0, errors.New("process tree root was not observed")
	}
	return total, nil
}
