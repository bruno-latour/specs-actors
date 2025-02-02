package miner

import (
	"errors"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/dline"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/specs-actors/v4/actors/util/adt"
)

// Returns deadline-related calculations for a deadline in some proving period and the current epoch.
func NewDeadlineInfo(periodStart abi.ChainEpoch, deadlineIdx uint64, currEpoch abi.ChainEpoch) *dline.Info {
	return dline.NewInfo(periodStart, deadlineIdx, currEpoch, WPoStPeriodDeadlines, WPoStProvingPeriod, WPoStChallengeWindow, WPoStChallengeLookback, FaultDeclarationCutoff)
}

func QuantSpecForDeadline(di *dline.Info) QuantSpec {
	return NewQuantSpec(WPoStProvingPeriod, di.Last())
}

// FindSector returns the deadline and partition index for a sector number.
// It returns an error if the sector number is not tracked by deadlines.
func FindSector(store adt.Store, deadlines *Deadlines, sectorNum abi.SectorNumber) (uint64, uint64, error) {
	for dlIdx := range deadlines.Due {
		dl, err := deadlines.LoadDeadline(store, uint64(dlIdx))
		if err != nil {
			return 0, 0, err
		}

		partitions, err := adt.AsArray(store, dl.Partitions, DeadlinePartitionsAmtBitwidth)
		if err != nil {
			return 0, 0, err
		}
		var partition Partition

		partIdx := uint64(0)
		stopErr := errors.New("stop")
		err = partitions.ForEach(&partition, func(i int64) error {
			found, err := partition.Sectors.IsSet(uint64(sectorNum))
			if err != nil {
				return err
			}
			if found {
				partIdx = uint64(i)
				return stopErr
			}
			return nil
		})
		if err == stopErr {
			return uint64(dlIdx), partIdx, nil
		} else if err != nil {
			return 0, 0, err
		}

	}
	return 0, 0, xerrors.Errorf("sector %d not due at any deadline", sectorNum)
}

// Returns true if the deadline at the given index is currently mutable. A
// "mutable" deadline may have new sectors assigned to it.
func deadlineIsMutable(provingPeriodStart abi.ChainEpoch, dlIdx uint64, currentEpoch abi.ChainEpoch) bool {
	// Get the next non-elapsed deadline (i.e., the next time we care about
	// mutations to the deadline).
	dlInfo := NewDeadlineInfo(provingPeriodStart, dlIdx, currentEpoch).NextNotElapsed()
	// Ensure that the current epoch is at least one challenge window before
	// that deadline opens.
	return currentEpoch < dlInfo.Open-WPoStChallengeWindow
}

// Returns true if optimistically accepted posts submitted to the given deadline
// may be disputed. Specifically, this ensures that:
//
// 1. Optimistic PoSts may not be disputed while the challenge window is open.
// 2. Optimistic PoSts may not be disputed after the miner could have compacted the deadline.
func deadlineAvailableForOptimisticPoStDispute(provingPeriodStart abi.ChainEpoch, dlIdx uint64, currentEpoch abi.ChainEpoch) bool {
	if provingPeriodStart > currentEpoch {
		// We haven't started proving yet, there's nothing to dispute.
		return false
	}
	dlInfo := NewDeadlineInfo(provingPeriodStart, dlIdx, currentEpoch).NextNotElapsed()

	return !dlInfo.IsOpen() && currentEpoch < (dlInfo.Close-WPoStProvingPeriod)+WPoStDisputeWindow
}

// Returns true if the given deadline may compacted in the current epoch.
// Deadlines may not be compacted when:
//
// 1. The deadline is currently being challenged.
// 2. The deadline is to be challenged next.
// 3. Optimistically accepted posts from the deadline's last challenge window
//    can currently be disputed.
func deadlineAvailableForCompaction(provingPeriodStart abi.ChainEpoch, dlIdx uint64, currentEpoch abi.ChainEpoch) bool {
	return deadlineIsMutable(provingPeriodStart, dlIdx, currentEpoch) &&
		!deadlineAvailableForOptimisticPoStDispute(provingPeriodStart, dlIdx, currentEpoch)
}
