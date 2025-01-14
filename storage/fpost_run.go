package storage

import (
	"context"
	"time"

	ffi "github.com/filecoin-project/filecoin-ffi"
	"go.opencensus.io/trace"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/actors"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/lib/sectorbuilder"
)

func (s *fpostScheduler) doPost(ctx context.Context, eps uint64, ts *types.TipSet) {
	ctx, abort := context.WithCancel(ctx)

	s.abort = abort
	s.activeEPS = eps

	go func() {
		defer abort()

		ctx, span := trace.StartSpan(ctx, "fpostScheduler.doPost")
		defer span.End()

		proof, err := s.runPost(ctx, eps, ts)
		if err != nil {
			log.Errorf("runPost failed: %+v", err)
			return
		}

		if err := s.submitPost(ctx, proof); err != nil {
			log.Errorf("submitPost failed: %+v", err)
			return
		}
	}()
}

func (s *fpostScheduler) runPost(ctx context.Context, eps uint64, ts *types.TipSet) (*actors.SubmitFallbackPoStParams, error) {
	ctx, span := trace.StartSpan(ctx, "storage.runPost")
	defer span.End()

	challengeRound := int64(eps + build.FallbackPoStDelay)

	rand, err := s.api.ChainGetRandomness(ctx, ts.Key(), challengeRound)
	if err != nil {
		return nil, xerrors.Errorf("failed to get chain randomness for fpost (ts=%d; eps=%d): %w", ts.Height(), eps, err)
	}

	ssi, err := s.sortedSectorInfo(ctx, ts)
	if err != nil {
		return nil, xerrors.Errorf("getting sorted sector info: %w", err)
	}

	log.Infow("running fPoSt", "chain-random", rand, "eps", eps, "height", ts.Height())

	tsStart := time.Now()
	var faults []uint64 // TODO

	var seed [32]byte
	copy(seed[:], rand)

	scandidates, proof, err := s.sb.GenerateFallbackPoSt(ssi, seed, faults)
	if err != nil {
		return nil, xerrors.Errorf("running post failed: %w", err)
	}

	elapsed := time.Since(tsStart)
	log.Infow("submitting PoSt", "pLen", len(proof), "elapsed", elapsed)

	candidates := make([]types.EPostTicket, len(scandidates))
	for i, sc := range scandidates {
		candidates[i] = types.EPostTicket{
			Partial:        sc.PartialTicket[:],
			SectorID:       sc.SectorID,
			ChallengeIndex: sc.SectorChallengeIndex,
		}
	}

	return &actors.SubmitFallbackPoStParams{
		Proof:      proof,
		Candidates: candidates,
	}, nil
}

func (s *fpostScheduler) sortedSectorInfo(ctx context.Context, ts *types.TipSet) (sectorbuilder.SortedPublicSectorInfo, error) {
	sset, err := s.api.StateMinerProvingSet(ctx, s.actor, ts)
	if err != nil {
		return sectorbuilder.SortedPublicSectorInfo{}, xerrors.Errorf("failed to get proving set for miner (tsH: %d): %w", ts.Height(), err)
	}
	if len(sset) == 0 {
		log.Warn("empty proving set! (ts.H: %d)", ts.Height())
	}

	sbsi := make([]ffi.PublicSectorInfo, len(sset))
	for k, sector := range sset {
		var commR [sectorbuilder.CommLen]byte
		copy(commR[:], sector.CommR)

		sbsi[k] = ffi.PublicSectorInfo{
			SectorID: sector.SectorID,
			CommR:    commR,
		}
	}

	return sectorbuilder.NewSortedPublicSectorInfo(sbsi), nil
}

func (s *fpostScheduler) submitPost(ctx context.Context, proof *actors.SubmitFallbackPoStParams) error {
	ctx, span := trace.StartSpan(ctx, "storage.commitPost")
	defer span.End()

	enc, aerr := actors.SerializeParams(proof)
	if aerr != nil {
		return xerrors.Errorf("could not serialize submit post parameters: %w", aerr)
	}

	msg := &types.Message{
		To:       s.actor,
		From:     s.worker,
		Method:   actors.MAMethods.SubmitFallbackPoSt,
		Params:   enc,
		Value:    types.NewInt(1000),     // currently hard-coded late fee in actor, returned if not late
		GasLimit: types.NewInt(10000000), // i dont know help
		GasPrice: types.NewInt(1),
	}

	// TODO: consider maybe caring about the output
	sm, err := s.api.MpoolPushMessage(ctx, msg)
	if err != nil {
		return xerrors.Errorf("pushing message to mpool: %w", err)
	}

	log.Infof("Submitted fallback post: %s", sm.Cid())

	return nil
}
