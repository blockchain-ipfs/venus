package message

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"time"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-filecoin/internal/pkg/block"
	"github.com/filecoin-project/go-filecoin/internal/pkg/types"
	"github.com/filecoin-project/go-filecoin/internal/pkg/vm/gas"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
)

const MinGasPremium = 100e3
const MaxSpendOnFeeDenom = 100

var (
	ReplaceByFeeRatioDefault  = 1.25
	MemPoolSizeLimitHiDefault = 30000
	MemPoolSizeLimitLoDefault = 20000
	PruneCooldownDefault      = time.Minute
	GasLimitOverestimation    = 1.25
)

func (ob *Outbox) GasEstimateFeeCap(ctx context.Context, msg *types.UnsignedMessage, maxqueueblks int64, _ block.TipSetKey) (types.AttoFIL, error) {
	tsKey := ob.chains.GetHead()
	ts, err := ob.chains.GetTipSet(tsKey)
	if err != nil {
		return types.NewGasFeeCap(0), err
	}

	parentBaseFee := ts.Blocks()[0].ParentBaseFee
	increaseFactor := math.Pow(1.+1./float64(types.BaseFeeMaxChangeDenom), float64(maxqueueblks))

	big.Add(parentBaseFee,big.NewInt(int64(increaseFactor*(1<<8))))
	feeInFuture := big.Add(parentBaseFee,big.NewInt(int64(increaseFactor*(1<<8))))
	out := big.Div(feeInFuture, big.NewInt(1<<8))

	if msg.GasPremium != big.Zero() {
		out = big.Add(out, msg.GasPremium)
	}

	return out, nil
}

type gasMeta struct {
	price types.AttoFIL
	limit int64
}

func medianGasPremium(prices []gasMeta, blocks int) abi.TokenAmount {
	sort.Slice(prices, func(i, j int) bool {
		// sort desc by price
		return prices[i].price.GreaterThan(prices[j].price)
	})

	at := types.BlockGasTarget * int64(blocks) / 2
	prev1, prev2 := big.Zero(), big.Zero()
	for _, price := range prices {
		prev1, prev2 = price.price, prev1
		at -= price.limit
		if at < 0 {
			break
		}
	}

	premium := prev1
	if prev2.Sign() != 0 {
		premium = big.Div(big.Add(prev1, prev2), big.NewInt(2))
	}

	return premium
}

func (ob *Outbox) GasEstimateGasPremium(ctx context.Context, nblocksincl uint64,
	sender address.Address, gaslimit int64, _ block.TipSetKey) (types.AttoFIL, error) {

	if nblocksincl == 0 {
		nblocksincl = 1
	}

	var prices []gasMeta
	var blocks int

	tsKey := ob.chains.GetHead()
	ts, err := ob.chains.GetTipSet(tsKey)
	if err != nil {
		return types.NewGasPremium(0), err
	}

	for i := uint64(0); i < nblocksincl*2; i++ {
		h, err := ts.Height()
		if err != nil {
			return types.NewGasPremium(0), err
		}
		if h == 0 {
			break // genesis
		}

		tsPKey, err := ts.Parents()
		if err != nil {
			return types.NewGasPremium(0), err
		}
		pts, err := ob.chains.GetTipSet(tsPKey)
		if err != nil {
			return types.NewGasPremium(0), err
		}

		blocks += len(pts.Blocks())
		msgs, err := ob.policy.MessagesForTipset(ctx,&pts)
		if err != nil {
			return types.NewGasPremium(0), xerrors.Errorf("loading messages: %w", err)
		}
		for _, msg := range msgs {
			prices = append(prices, gasMeta{
				price: msg.VMMessage().GasPremium,
				limit: int64(msg.VMMessage().GasLimit),
			})
		}

		ts = pts
	}

	premium := medianGasPremium(prices, blocks)

	if big.Cmp(premium, big.NewInt(MinGasPremium)) < 0 {
		switch nblocksincl {
		case 1:
			premium = big.NewInt(2 * MinGasPremium)
		case 2:
			premium = big.NewInt(1.5 * MinGasPremium)
		default:
			premium = big.NewInt(MinGasPremium)
		}
	}

	// add some noise to normalize behaviour of message selection
	const precision = 32
	// mean 1, stddev 0.005 => 95% within +-1%
	noise := 1 + rand.NormFloat64()*0.005
	premium = big.Mul(premium, big.NewInt(int64(noise*(1<<precision))+1))
	premium = big.Div(premium, big.NewInt(1<<precision))
	return premium, nil
}

func (ob *Outbox) GasEstimateGasLimit(ctx context.Context, msgIn *types.UnsignedMessage, _ block.TipSetKey) (int64, error) {

	msg := *msgIn
	msg.GasLimit = types.BlockGasLimit
	msg.GasFeeCap = big.NewInt(int64(types.MinimumBaseFee) + 1)
	msg.GasPremium = big.NewInt(1)

	tsKey := ob.chains.GetHead()

	actor,err := ob.actors.GetActorAt(ctx, tsKey, msgIn.From)
	if err != nil {
		return -1, xerrors.Errorf("getting key address: %w", err)
	}

	// Sender should not be an empty actor
	if actor == nil || actor.Empty() {
		return -1, xerrors.Errorf("sender %s is missing/empty", msg.From)
	}

	// todo 从Queue中查询出from所有未上链的消息 ???
	//pending, ts := a.Mpool.PendingFor(fromA)
	//priorMsgs := make([]types.ChainMsg, 0, len(pending))
	//for _, m := range pending {
	//	priorMsgs = append(priorMsgs, m)
	//}
	//
	//res, err := ob.Stmgr.CallWithGas(ctx, &msg, priorMsgs, ts)
	//if err != nil {
	//	return -1, xerrors.Errorf("CallWithGas failed: %w", err)
	//}
	//if res.MsgRct.ExitCode != exitcode.Ok {
	//	return -1, xerrors.Errorf("message execution failed: exit %s, reason: %s", res.MsgRct.ExitCode, res.Error)
	//}
	//
	//// Special case for PaymentChannel collect, which is deleting actor
	//st, err := ob.Stmgr.ParentState(ts)
	//if err != nil {
	//	_ = err
	//	// somewhat ignore it as it can happen and we just want to detect
	//	// an existing PaymentChannel actor
	//	return res.MsgRct.GasUsed, nil
	//}
	//act, err := st.GetActor(msg.To)
	//if err != nil {
	//	_ = err
	//	// somewhat ignore it as it can happen and we just want to detect
	//	// an existing PaymentChannel actor
	//	return res.MsgRct.GasUsed, nil
	//}
	//
	//if !act.IsPaymentChannelActor() {
	//	return res.MsgRct.GasUsed, nil
	//}
	//if msgIn.Method != builtin0.MethodsPaych.Collect {
	//	return res.MsgRct.GasUsed, nil
	//}
	//
	//// return GasUsed without the refund for DestoryActor
	//return res.MsgRct.GasUsed + 76e3, nil
	return 0, nil
}

const FilecoinPrecision = uint64(1_000_000_000_000_000_000)

type MessageSendSpec struct {
	MaxFee abi.TokenAmount
}

var DefaultMessageSendSpec = MessageSendSpec{
	// MaxFee of 0.1FIL
	MaxFee: abi.NewTokenAmount(int64(FilecoinPrecision) / 10),
}

func (ms *MessageSendSpec) Get() MessageSendSpec {
	if ms == nil {
		return DefaultMessageSendSpec
	}

	return *ms
}

func (ob *Outbox) GasEstimateMessageGas(ctx context.Context, msg *types.UnsignedMessage, spec *MessageSendSpec, _ block.TipSetKey) (*types.UnsignedMessage, error) {
	if msg.GasLimit == 0 {
		gasLimit, err := ob.GasEstimateGasLimit(ctx, msg, block.TipSetKey{})
		if err != nil {
			return nil, xerrors.Errorf("estimating gas used: %w", err)
		}
		msg.GasLimit = gas.NewGas(int64(float64(gasLimit) * GasLimitOverestimation))
	}

	if msg.GasPremium == types.ZeroAttoFIL || big.Cmp(msg.GasPremium, big.NewInt(0)) == 0 {
		gasPremium, err := ob.GasEstimateGasPremium(ctx, 2, msg.From, int64(msg.GasLimit), block.TipSetKey{})
		if err != nil {
			return nil, xerrors.Errorf("estimating gas price: %w", err)
		}
		msg.GasPremium = gasPremium
	}

	if msg.GasFeeCap == types.ZeroAttoFIL || big.Cmp(msg.GasFeeCap, big.NewInt(0)) == 0 {
		feeCap, err := ob.GasEstimateFeeCap(ctx, msg, 20, block.TipSetKey{})
		if err != nil {
			return nil, xerrors.Errorf("estimating fee cap: %w", err)
		}
		msg.GasFeeCap = feeCap
	}

	return msg, nil
}