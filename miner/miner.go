package miner

import (
	"context"
	"sync"
	"time"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/actors"
	"github.com/filecoin-project/lotus/chain/address"
	"github.com/filecoin-project/lotus/chain/gen"
	"github.com/filecoin-project/lotus/chain/types"

	logging "github.com/ipfs/go-log"
	"go.opencensus.io/trace"
	"golang.org/x/xerrors"
)

var log = logging.Logger("miner")

type waitFunc func(ctx context.Context) error

func NewMiner(api api.FullNode, epp gen.ElectionPoStProver) *Miner {
	return &Miner{
		api: api,
		epp: epp,
		waitFunc: func(ctx context.Context) error {
			// Wait around for half the block time in case other parents come in
			time.Sleep(build.BlockDelay * time.Second / 2)
			return nil
		},
	}
}

type Miner struct {
	api api.FullNode

	epp gen.ElectionPoStProver

	lk        sync.Mutex
	addresses []address.Address
	stop      chan struct{}
	stopping  chan struct{}

	waitFunc waitFunc

	lastWork *MiningBase
}

func (m *Miner) Addresses() ([]address.Address, error) {  // 返回一个地址空数组，并没有地址
	m.lk.Lock()
	defer m.lk.Unlock()

	out := make([]address.Address, len(m.addresses))
	copy(out, m.addresses)

	return out, nil
}

func (m *Miner) Register(addr address.Address) error {
	m.lk.Lock()
	defer m.lk.Unlock()

	if len(m.addresses) > 0 {   // 先检查地址数量，不为0时
		for _, a := range m.addresses {  // 如果存在两个及以上的地址  或者  第一个地址与传进来的地址不相符，返回错误
			if a == addr {
				log.Warnf("miner.Register called more than once for actor '%s'", addr)
				return xerrors.Errorf("miner.Register called more than once for actor '%s'", addr)
			}
		}
	}

	m.addresses = append(m.addresses, addr) // 叠加地址到数组中
	if len(m.addresses) == 1 {
		m.stop = make(chan struct{})
		go m.mine(context.TODO())
	}

	return nil
}

func (m *Miner) Unregister(ctx context.Context, addr address.Address) error {
	m.lk.Lock()
	defer m.lk.Unlock()
	if len(m.addresses) == 0 {
		return xerrors.New("no addresses registered")
	}

	idx := -1

	for i, a := range m.addresses {
		if a == addr {
			idx = i
			break
		}
	}
	if idx == -1 {
		return xerrors.New("unregister: address not found")
	}

	m.addresses[idx] = m.addresses[len(m.addresses)-1]
	m.addresses = m.addresses[:len(m.addresses)-1]

	// Unregistering last address, stop mining first
	if len(m.addresses) == 0 && m.stop != nil {
		m.stopping = make(chan struct{})
		stopping := m.stopping
		close(m.stop)

		select {
		case <-stopping:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

func (m *Miner) mine(ctx context.Context) {
	ctx, span := trace.StartSpan(ctx, "/mine")  // 添加"/mine"字段到ctx中
	defer span.End()

	var lastBase MiningBase

eventLoop:
	for {  // 挖块死循环
		select {  // 退出信号
		case <-m.stop:
			stopping := m.stopping
			m.stop = nil
			m.stopping = nil
			close(stopping)
			return

		default:
		}

		m.lk.Lock()
		addrs := m.addresses
		m.lk.Unlock()

		// Sleep a small amount in order to wait for other blocks to arrive
		if err := m.waitFunc(ctx); err != nil { // 为了等待其他区块的到来，半个区块时间的睡眠
			log.Error(err)
			return
		}

		base, err := m.GetBestMiningCandidate(ctx)   // 获取最好的矿工候选者!!!!!!!!!!!!!!!!重点!!!!!!!!!!!!!!!!!!!!
		if err != nil {
			log.Errorf("failed to get best mining candidate: %s", err)
			continue
		}
		if base.ts.Equals(lastBase.ts) && lastBase.nullRounds == base.nullRounds {
			log.Warnf("BestMiningCandidate from the previous round: %s (nulls:%d)", lastBase.ts.Cids(), lastBase.nullRounds)
			time.Sleep(build.BlockDelay * time.Second)
			continue
		}
		lastBase = *base

		blks := make([]*types.BlockMsg, 0)

		for _, addr := range addrs {
			b, err := m.mineOne(ctx, addr, base)  // 调用mineOne函数创建新的区块!!!!!!!!!!!!!!!重点更重点!!!!!!!!!!!!!!!!!!
			if err != nil {
				log.Errorf("mining block failed: %+v", err)
				continue
			}
			if b != nil {
				blks = append(blks, b)
			}
		}

		if len(blks) != 0 {  // 出块有效
			btime := time.Unix(int64(blks[0].Header.Timestamp), 0)  // 获取出块时间
			if time.Now().Before(btime) {  // 如果 即刻时间 < 出块时间
				time.Sleep(time.Until(btime))   // 睡眠到出块时间
			} else {
				log.Warnw("mined block in the past", "block-time", btime,
					"time", time.Now(), "duration", time.Now().Sub(btime))
			}

			mWon := make(map[address.Address]struct{})
			for _, b := range blks {
				_, notOk := mWon[b.Header.Miner]
				if notOk {
					log.Errorw("2 blocks for the same miner. Throwing hands in the air. Report this. It is important.", "bloks", blks)
					continue eventLoop
				}
				mWon[b.Header.Miner] = struct{}{}
			}
			for _, b := range blks {
				if err := m.api.SyncSubmitBlock(ctx, b); err != nil {  // 同步确认区块
					log.Errorf("failed to submit newly mined block: %s", err)
				}
			}
		} else {  // 出块无效
			nextRound := time.Unix(int64(base.ts.MinTimestamp()+uint64(build.BlockDelay*base.nullRounds)), 0)  // 获取下一轮出块时间
			time.Sleep(time.Until(nextRound))  // 睡眠到下一轮出块时间
		}
	}
}

type MiningBase struct {
	ts         *types.TipSet
	nullRounds uint64
}

func (m *Miner) GetBestMiningCandidate(ctx context.Context) (*MiningBase, error) {  // 产生最好矿工候选者
	bts, err := m.api.ChainHead(ctx)   // 获取选票权重  func (a *ChainAPI) ChainHead -> func (cs *ChainStore) GetHeaviestTipSet  -> heaviest   *types.TipSet
	if err != nil {
		return nil, err
	}

	if m.lastWork != nil {
		if m.lastWork.ts.Equals(bts) {
			return m.lastWork, nil
		}

		btsw, err := m.api.ChainTipSetWeight(ctx, bts)   // 设置本次选票权重    func (cs *ChainStore) Weight
		if err != nil {
			return nil, err
		}
		ltsw, err := m.api.ChainTipSetWeight(ctx, m.lastWork.ts)  // 设置累积选票权重
		if err != nil {
			return nil, err
		}

		if types.BigCmp(btsw, ltsw) <= 0 {  // 比较两个选票权重    func BigCmp(a, b BigInt)
			return m.lastWork, nil
		}
	}

	return &MiningBase{
		ts: bts,
	}, nil
}

func (m *Miner) hasPower(ctx context.Context, addr address.Address, ts *types.TipSet) (bool, error) {
	power, err := m.api.StateMinerPower(ctx, addr, ts)
	if err != nil {
		return false, err
	}

	return power.MinerPower.Equals(types.NewInt(0)), nil
}

func (m *Miner) mineOne(ctx context.Context, addr address.Address, base *MiningBase) (*types.BlockMsg, error) {
	log.Debugw("attempting to mine a block", "tipset", types.LogCids(base.ts.Cids()))
	start := time.Now()

	hasPower, err := m.hasPower(ctx, addr, base.ts)
	if err != nil {
		return nil, xerrors.Errorf("checking if miner is slashed: %w", err)
	}
	if hasPower {
		// slashed or just have no power yet
		base.nullRounds++
		return nil, nil
	}

	ticket, err := m.computeTicket(ctx, addr, base)
	if err != nil {
		return nil, xerrors.Errorf("scratching ticket failed: %w", err)
	}

	win, proof, err := gen.IsRoundWinner(ctx, base.ts, int64(base.ts.Height()+base.nullRounds+1), addr, m.epp, m.api)  // 产生本轮胜利者!!!!!!!!重点!!!!!!!
	if err != nil {
		return nil, xerrors.Errorf("failed to check if we win next round: %w", err)
	}

	if !win { // 如果没有胜出
		base.nullRounds++
		return nil, nil
	}

	b, err := m.createBlock(base, addr, ticket, proof)  // 到这里已经胜出了，创建区块
	if err != nil {
		return nil, xerrors.Errorf("failed to create block: %w", err)
	}
	log.Infow("mined new block", "cid", b.Cid(), "height", b.Header.Height)

	dur := time.Now().Sub(start)
	log.Infof("Creating block took %s", dur)
	if dur > time.Second*build.BlockDelay {
		log.Warn("CAUTION: block production took longer than the block delay. Your computer may not be fast enough to keep up")
	}

	return b, nil
}

func (m *Miner) computeVRF(ctx context.Context, addr address.Address, input []byte) ([]byte, error) {
	w, err := m.getMinerWorker(ctx, addr, nil)
	if err != nil {
		return nil, err
	}

	return gen.ComputeVRF(ctx, m.api.WalletSign, w, addr, gen.DSepTicket, input)
}

func (m *Miner) getMinerWorker(ctx context.Context, addr address.Address, ts *types.TipSet) (address.Address, error) {
	ret, err := m.api.StateCall(ctx, &types.Message{
		From:   addr,
		To:     addr,
		Method: actors.MAMethods.GetWorkerAddr,
	}, ts)
	if err != nil {
		return address.Undef, xerrors.Errorf("failed to get miner worker addr: %w", err)
	}

	if ret.ExitCode != 0 {
		return address.Undef, xerrors.Errorf("failed to get miner worker addr (exit code %d)", ret.ExitCode)
	}

	w, err := address.NewFromBytes(ret.Return)
	if err != nil {
		return address.Undef, xerrors.Errorf("GetWorkerAddr returned malformed address: %w", err)
	}

	return w, nil
}

func (m *Miner) computeTicket(ctx context.Context, addr address.Address, base *MiningBase) (*types.Ticket, error) {

	vrfBase := base.ts.MinTicket().VRFProof

	vrfOut, err := m.computeVRF(ctx, addr, vrfBase)
	if err != nil {
		return nil, err
	}

	return &types.Ticket{
		VRFProof: vrfOut,
	}, nil
}

func (m *Miner) createBlock(base *MiningBase, addr address.Address, ticket *types.Ticket, proof *types.EPostProof) (*types.BlockMsg, error) {

	pending, err := m.api.MpoolPending(context.TODO(), base.ts)
	if err != nil {
		return nil, xerrors.Errorf("failed to get pending messages: %w", err)
	}

	msgs, err := selectMessages(context.TODO(), m.api.StateGetActor, base, pending)
	if err != nil {
		return nil, xerrors.Errorf("message filtering failed: %w", err)
	}

	uts := base.ts.MinTimestamp() + uint64(build.BlockDelay*(base.nullRounds+1))

	nheight := base.ts.Height() + base.nullRounds + 1

	// why even return this? that api call could just submit it for us
	return m.api.MinerCreateBlock(context.TODO(), addr, base.ts, ticket, proof, msgs, nheight, uint64(uts))
}

type actorLookup func(context.Context, address.Address, *types.TipSet) (*types.Actor, error)

func selectMessages(ctx context.Context, al actorLookup, base *MiningBase, msgs []*types.SignedMessage) ([]*types.SignedMessage, error) {
	out := make([]*types.SignedMessage, 0, len(msgs))
	inclNonces := make(map[address.Address]uint64)
	inclBalances := make(map[address.Address]types.BigInt)
	for _, msg := range msgs {
		if msg.Message.To == address.Undef {
			log.Warnf("message in mempool had bad 'To' address")
			continue
		}

		from := msg.Message.From
		act, err := al(ctx, from, base.ts)
		if err != nil {
			return nil, xerrors.Errorf("failed to check message sender balance: %w", err)
		}

		if _, ok := inclNonces[from]; !ok {
			inclNonces[from] = act.Nonce
			inclBalances[from] = act.Balance
		}

		if inclBalances[from].LessThan(msg.Message.RequiredFunds()) {
			log.Warnf("message in mempool does not have enough funds: %s", msg.Cid())
			continue
		}

		if msg.Message.Nonce > inclNonces[from] {
			log.Warnf("message in mempool has too high of a nonce (%d > %d) %s", msg.Message.Nonce, inclNonces[from], msg.Cid())
			continue
		}

		if msg.Message.Nonce < inclNonces[from] {
			log.Warnf("message in mempool has already used nonce (%d < %d), from %s, to %s, %s", msg.Message.Nonce, inclNonces[from], msg.Message.From, msg.Message.To, msg.Cid())
			continue
		}

		inclNonces[from] = msg.Message.Nonce + 1
		inclBalances[from] = types.BigSub(inclBalances[from], msg.Message.RequiredFunds())

		out = append(out, msg)
	}
	return out, nil
}
