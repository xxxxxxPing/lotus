package storage

import (
	"context"
	"io"

	cid "github.com/ipfs/go-cid"
	xerrors "golang.org/x/xerrors"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/lib/padreader"
	"github.com/filecoin-project/lotus/lib/sectorbuilder"
)

type TicketFn func(context.Context) (*sectorbuilder.SealTicket, error)

type SealTicket struct {
	BlockHeight uint64
	TicketBytes []byte
}

func (t *SealTicket) SB() sectorbuilder.SealTicket {
	out := sectorbuilder.SealTicket{BlockHeight: t.BlockHeight}
	copy(out.TicketBytes[:], t.TicketBytes)
	return out
}

type SealSeed struct {
	BlockHeight uint64
	TicketBytes []byte
}

func (t *SealSeed) SB() sectorbuilder.SealSeed {
	out := sectorbuilder.SealSeed{BlockHeight: t.BlockHeight}
	copy(out.TicketBytes[:], t.TicketBytes)
	return out
}

type Piece struct {
	DealID uint64

	Size  uint64
	CommP []byte
}

func (p *Piece) ppi() (out sectorbuilder.PublicPieceInfo) {
	out.Size = p.Size
	copy(out.CommP[:], p.CommP)
	return out
}

type SectorInfo struct {
	State    api.SectorState
	SectorID uint64

	// Packing

	Pieces []Piece

	// PreCommit
	CommC     []byte
	CommD     []byte
	CommR     []byte
	CommRLast []byte
	Proof     []byte
	Ticket    SealTicket

	PreCommitMessage *cid.Cid

	// PreCommitted
	Seed SealSeed

	// Committing
	CommitMessage *cid.Cid
}

type sectorUpdate struct {
	newState api.SectorState
	id       uint64
	err      error
	mut      func(*SectorInfo)
}

func (t *SectorInfo) pieceInfos() []sectorbuilder.PublicPieceInfo {
	out := make([]sectorbuilder.PublicPieceInfo, len(t.Pieces))
	for i, piece := range t.Pieces {
		out[i] = piece.ppi()
	}
	return out
}

func (t *SectorInfo) deals() []uint64 {
	out := make([]uint64, len(t.Pieces))
	for i, piece := range t.Pieces {
		out[i] = piece.DealID
	}
	return out
}

func (t *SectorInfo) existingPieces() []uint64 {
	out := make([]uint64, len(t.Pieces))
	for i, piece := range t.Pieces {
		out[i] = piece.Size
	}
	return out
}

func (t *SectorInfo) rspco() sectorbuilder.RawSealPreCommitOutput {
	var out sectorbuilder.RawSealPreCommitOutput

	copy(out.CommC[:], t.CommC)
	copy(out.CommD[:], t.CommD)
	copy(out.CommR[:], t.CommR)
	copy(out.CommRLast[:], t.CommRLast)

	return out
}

func (m *Miner) sectorStateLoop(ctx context.Context) error {
	trackedSectors, err := m.ListSectors()
	if err != nil {
		return err
	}

	go func() {
		for _, si := range trackedSectors {
			select {
			case m.sectorUpdated <- sectorUpdate{
				newState: si.State,
				id:       si.SectorID,
				err:      nil,
				mut:      nil,
			}:
			case <-ctx.Done():
				log.Warn("didn't restart processing for all sectors: ", ctx.Err())
				return
			}
		}
	}()

	{
		// verify on-chain state
		trackedByID := map[uint64]*SectorInfo{}
		for _, si := range trackedSectors {
			i := si
			trackedByID[si.SectorID] = &i
		}

		curTs, err := m.api.ChainHead(ctx)
		if err != nil {
			return xerrors.Errorf("getting chain head: %w", err)
		}

		ps, err := m.api.StateMinerProvingSet(ctx, m.maddr, curTs)
		if err != nil {
			return err
		}
		for _, ocs := range ps {
			if _, ok := trackedByID[ocs.SectorID]; ok {
				continue // TODO: check state
			}

			// TODO: attempt recovery
			log.Warnf("untracked sector %d found on chain", ocs.SectorID)
		}
	}

	go func() {
		defer log.Warn("quitting deal provider loop")
		defer close(m.stopped)

		for {
			select {
			case sector := <-m.sectorIncoming:
				m.onSectorIncoming(sector)
			case update := <-m.sectorUpdated:
				m.onSectorUpdated(ctx, update)
			case <-m.stop:
				return
			}
		}
	}()

	return nil
}

func (m *Miner) onSectorIncoming(sector *SectorInfo) {
	has, err := m.sectors.Has(sector.SectorID)
	if err != nil {
		return
	}
	if has {
		log.Warnf("SealPiece called more than once for sector %d", sector.SectorID)
		return
	}

	if err := m.sectors.Begin(sector.SectorID, sector); err != nil {
		// We may have re-sent the proposal
		log.Errorf("deal tracking failed: %s", err)
		m.failSector(sector.SectorID, err)
		return
	}

	go func() {
		select {
		case m.sectorUpdated <- sectorUpdate{
			newState: api.Packing,
			id:       sector.SectorID,
		}:
		case <-m.stop:
			log.Warn("failed to send incoming sector update, miner shutting down")
		}
	}()
}

func (m *Miner) onSectorUpdated(ctx context.Context, update sectorUpdate) {
	log.Infof("Sector %d updated state to %s", update.id, api.SectorStateStr(update.newState))
	var sector SectorInfo
	err := m.sectors.Mutate(update.id, func(s *SectorInfo) error {
		s.State = update.newState
		if update.mut != nil {
			update.mut(s)
		}
		sector = *s
		return nil
	})
	if update.err != nil {
		log.Errorf("sector %d failed: %s", update.id, update.err)
		m.failSector(update.id, update.err)
		return
	}
	if err != nil {
		m.failSector(update.id, err)
		return
	}

	switch update.newState {
	case api.Packing:
		m.handleSectorUpdate(ctx, sector, m.finishPacking, api.Unsealed)
	case api.Unsealed:
		m.handleSectorUpdate(ctx, sector, m.sealPreCommit, api.PreCommitting)
	case api.PreCommitting:
		m.handleSectorUpdate(ctx, sector, m.preCommit, api.PreCommitted)
	case api.PreCommitted:
		m.handleSectorUpdate(ctx, sector, m.preCommitted, api.SectorNoUpdate)
	case api.Committing:
		m.handleSectorUpdate(ctx, sector, m.committing, api.Proving)
	case api.Proving:
		// TODO: track sector health / expiration
		log.Infof("Proving sector %d", update.id)
	case api.SectorNoUpdate: // noop
	default:
		log.Errorf("unexpected sector update state: %d", update.newState)
	}
}

func (m *Miner) failSector(id uint64, err error) {
	log.Errorf("sector %d error: %+v", id, err)
}

func (m *Miner) AllocatePiece(size uint64) (sectorID uint64, offset uint64, err error) {
	if padreader.PaddedSize(size) != size {
		return 0, 0, xerrors.Errorf("cannot allocate unpadded piece")
	}

	sid, err := m.sb.AcquireSectorId() // TODO: Put more than one thing in a sector
	if err != nil {
		return 0, 0, xerrors.Errorf("acquiring sector ID: %w", err)
	}

	// offset hard-coded to 0 since we only put one thing in a sector for now
	return sid, 0, nil
}

func (m *Miner) SealPiece(ctx context.Context, size uint64, r io.Reader, sectorID uint64, dealID uint64) error {
	log.Infof("Seal piece for deal %d", dealID)

	ppi, err := m.sb.AddPiece(size, sectorID, r, []uint64{})
	if err != nil {
		return xerrors.Errorf("adding piece to sector: %w", err)
	}

	return m.newSector(ctx, sectorID, dealID, ppi)
}

func (m *Miner) newSector(ctx context.Context, sid uint64, dealID uint64, ppi sectorbuilder.PublicPieceInfo) error {
	si := &SectorInfo{
		SectorID: sid,

		Pieces: []Piece{
			{
				DealID: dealID,

				Size:  ppi.Size,
				CommP: ppi.CommP[:],
			},
		},
	}
	select {
	case m.sectorIncoming <- si:
		return nil
	case <-ctx.Done():
		return xerrors.Errorf("failed to submit sector for sealing, queue full: %w", ctx.Err())
	}
}
