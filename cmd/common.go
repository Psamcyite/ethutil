package cmd

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"regexp"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/shopspring/decimal"
)

var ethAddressRE = regexp.MustCompile("^(0x)?[0-9a-fA-F]{40}$")

// contains returns true if array arr contains str.
func contains(arr []string, str string) bool {
	for _, a := range arr {
		if a == str {
			return true
		}
	}
	return false
}

// checkErr panic if err != nil.
func checkErr(err error) {
	if err != nil {
		if rpcErr, ok := err.(rpc.DataError); ok {
			var errData = rpcErr.ErrorData()
			log.Printf("data field in error: %v", errData)
			if errData != nil {
				if errStr, ok := errData.(string); ok && len(errStr) >= 10 {
					var funcHash = errStr[0:10]
					funcSig, err := GetFuncSig(funcHash)
					if err != nil {
						log.Printf("getFuncSig failed %v", err)
					}
					for _, data := range funcSig {
						log.Printf("%s is signature of %s", funcHash, data)
					}
				}
			}
		}
		log.Fatalf("%v", err)
		// panic(err)
	}
}

// isValidEthAddress returns true if v is a valid eth address.
func isValidEthAddress(v string) bool {
	return ethAddressRE.MatchString(v)
}

// isContractAddress returns true if address is a valid eth contract address.
func isContractAddress(client *ethclient.Client, address common.Address) (bool, error) {
	bytecode, err := client.CodeAt(context.Background(), address, nil) // nil is latest block
	if err != nil {
		return false, err
	}

	isContract := len(bytecode) > 0
	return isContract, nil
}

// has0xPrefix returns true if str starts with 0x or 0X.
func has0xPrefix(str string) bool {
	return len(str) >= 2 && str[0] == '0' && (str[1] == 'x' || str[1] == 'X')
}

// isValidHexString returns true if str is a valid hex string or empty string.
func isValidHexString(str string) bool {
	if str == "" {
		return true
	}
	var hexWithout0x = str
	if has0xPrefix(str) {
		hexWithout0x = str[2:]
	}
	_, err := hex.DecodeString(hexWithout0x)
	if err != nil {
		return false
	}

	return true
}

// bigInt2Decimal converts x from big.Int to decimal.Decimal.
func bigInt2Decimal(x *big.Int) decimal.Decimal {
	if x == nil {
		return decimal.New(0, 0)
	}
	return decimal.NewFromBigInt(x, 0)
}

// buildPrivateKeyFromHex builds ecdsa.PrivateKey from hex string (the leading 0x is optional),
// it would panic if input an invalid hex string.
func buildPrivateKeyFromHex(privateKeyHex string) *ecdsa.PrivateKey {
	if has0xPrefix(privateKeyHex) {
		privateKeyHex = privateKeyHex[2:] // remove leading 0x
	}

	privateKey, err := crypto.HexToECDSA(privateKeyHex)
	checkErr(err)

	return privateKey
}

// wei2Other converts wei to other unit (specified by targetUnit).
func wei2Other(sourceAmtInWei decimal.Decimal, targetUnit string) decimal.Decimal {
	decimal.DivisionPrecision = 18
	if targetUnit == unitWei {
		return sourceAmtInWei
	} else if targetUnit == unitGwei {
		return sourceAmtInWei.Div(decimal.NewFromBigInt(big.NewInt(1), 9))
	} else if targetUnit == unitEther {
		return sourceAmtInWei.Div(decimal.NewFromBigInt(big.NewInt(1), 18))
	} else {
		panic(fmt.Sprintf("unrecognized unit %v", targetUnit))
	}
}

// unify2Wei converts any unit (specified by sourceUnit) to wei.
func unify2Wei(sourceAmt decimal.Decimal, sourceUnit string) decimal.Decimal {
	if sourceUnit == unitWei {
		return sourceAmt
	} else if sourceUnit == unitGwei {
		return sourceAmt.Mul(decimal.NewFromBigInt(big.NewInt(1), 9))
	} else if sourceUnit == unitEther {
		return sourceAmt.Mul(decimal.NewFromBigInt(big.NewInt(1), 18))
	} else {
		panic(fmt.Sprintf("unrecognized unit %v", sourceUnit))
	}
}

// extractAddressFromPrivateKey extracts address from ecdsa.PrivateKey.
func extractAddressFromPrivateKey(privateKey *ecdsa.PrivateKey) common.Address {
	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		panic("cannot assert type: publicKey is not of type *ecdsa.PublicKey")
	}

	return crypto.PubkeyToAddress(*publicKeyECDSA)
}

// getTxReceipt gets the receipt of tx, re-check util timeout if tx not found.
func getTxReceipt(client *ethclient.Client, txHash common.Hash, timeout time.Duration) (*types.Receipt, error) {
	var beginTime = time.Now()

recheck:
	if rp, err := client.TransactionReceipt(context.Background(), txHash); err != nil {
		if err == ethereum.NotFound {
			log.Printf("tx %v not found (may be pending) in network", txHash.String())
		} else {
			return nil, fmt.Errorf("TransactionReceipt fail: %w", err)
		}
	} else {
		// no error
		return rp, nil
	}

	if timeout > 0 && beginTime.Add(timeout).After(time.Now()) {
		// timeout
		return nil, fmt.Errorf("GetReceipt timeout")
	}

	// not timeout
	log.Printf("re-check tx %v after 5 seconds", txHash.String())
	time.Sleep(time.Second * 5)
	goto recheck
}

const EthGasStationUrl = "https://ethgasstation.info/json/ethgasAPI.json"

// GasStationPrice, the struct of response of EthGasStationUrl
type GasStationPrice struct {
	Fast        float64
	Fastest     float64
	SafeLow     float64
	Average     float64
	SafeLowWait float64
	AvgWait     float64
	FastWait    float64
	FastestWait float64
}

// getGasPrice, get gas price from EthGasStationUrl, built-in method client.SuggestGasPrice is not good enough.
func getGasPriceFromEthGasStation() (*big.Int, error) {
	var gasStationPrice GasStationPrice
	resp, err := http.Get(EthGasStationUrl)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(body, &gasStationPrice)
	if err != nil {
		return nil, err
	}

	// we use `fast`
	gasPrice := big.NewInt(int64(gasStationPrice.Fast * 100000000))
	return gasPrice, nil
}

// GenRawTx return raw tx, a hex string with 0x prefix
func GenRawTx(signedTx *types.Transaction) (string, error) {
	data, err := signedTx.MarshalBinary()
	if err != nil {
		return "", err
	}

	return hexutil.Encode(data), nil
}

// SendRawTransaction broadcast signed tx and return tx returned by rpc node
func SendRawTransaction(rpcClient *rpc.Client, signedTx *types.Transaction) (*common.Hash, error) {
	rawTx, err := GenRawTx(signedTx)
	if err != nil {
		return nil, err
	}

	var result hexutil.Bytes
	err = rpcClient.CallContext(context.Background(), &result, "eth_sendRawTransaction", rawTx)
	if err != nil {
		return nil, err
	}

	var hash = common.HexToHash(hexutil.Encode(result))
	return &hash, nil
}

// Transact invokes the (paid) contract method.
func Transact(rpcClient *rpc.Client, client *ethclient.Client, privateKey *ecdsa.PrivateKey, toAddress *common.Address, amount *big.Int, gasPrice *big.Int, data []byte) (string, error) {
	fromAddress := extractAddressFromPrivateKey(privateKey)

	var nonce uint64
	var err error
	if globalOptNonce < 0 {
		nonce, err = client.PendingNonceAt(context.Background(), fromAddress)
		if err != nil {
			return "", fmt.Errorf("PendingNonceAt fail: %w", err)
		}
	} else {
		nonce = uint64(globalOptNonce)
	}

	gasLimit := globalOptGasLimit
	if gasLimit == 0 { // if user not specified
		gasLimit = uint64(gasUsedByTransferEth)

		if toAddress == nil {
			gasLimit = 7000000 // the default gas limit for deploy contract, can be overwrite by option
		} else {
			isContract, err := isContractAddress(client, *toAddress)
			if err != nil {
				return "", fmt.Errorf("isContractAddress fail: %w", err)
			}
			if isContract { // gasUsedByTransferEth may be not enough if send to contract
				gasLimit = 900000
			}
			if len(data) > 0 { // gasUsedByTransferEth may be not enough if with payload data
				gasLimit = 900000
			}
		}
	}

	// if not specified
	if gasPrice == nil {
		gasPrice, err = getGasPrice(globalClient.EthClient)
		checkErr(err)
	}

	var tx *types.Transaction

	if globalOptTxType == txTypeEip1559 {
		var maxPriorityFeePerGasEstimate = new(big.Int)
		var maxFeePerGasEstimate = new(big.Int)
		if globalOptMaxPriorityFeePerGas == "" || globalOptMaxFeePerGas == "" {
			// Use rpc eth_feeHistory to estimate default maxPriorityFeePerGas and maxFeePerGas
			// See https://docs.alchemy.com/docs/how-to-build-a-gas-fee-estimator-using-eip-1559
			//
			// $ curl -X POST --data '{ "id": 1, "jsonrpc": "2.0", "method": "eth_feeHistory", "params": ["0x4", "latest", [5, 50, 95]] }' https://mainnet.infura.io/v3/21a9f5ba4bce425795cac796a66d7472
			// {
			//  "jsonrpc": "2.0",
			//  "id": 1,
			//  "result": {
			//    "baseFeePerGas": [
			//      "0x4ed3ef336",
			//      "0x4d2c282cd",
			//      "0x4db586991",
			//      "0x4d8275e8e",
			//      "0x4b5fb0a47"
			//    ],
			//    "gasUsedRatio": [
			//      0.41600023333333336,
			//      0.5278128666666667,
			//      0.4897323,
			//      0.3897776666666667
			//    ],
			//    "oldestBlock": "0xffc0a9",
			//    "reward": [
			//      [
			//        "0x6b51f67",
			//        "0x3b9aca00",
			//        "0x106853ddd8"
			//      ],
			//      [
			//        "0xa9970dc",
			//        "0x1dcd6500",
			//        "0x10abffd64"
			//      ],
			//      [
			//        "0x6190547",
			//        "0x1dcd6500",
			//        "0x9becf3d3c"
			//      ],
			//      [
			//        "0x94a104a",
			//        "0x1dcd6500",
			//        "0x1032d8cdb"
			//      ]
			//    ]
			//  }
			// }
			feeHistory, err := client.FeeHistory(context.Background(), 4, nil, []float64{5, 50, 95})
			checkErr(err)
			var slow big.Int
			slow.Add(feeHistory.Reward[0][0], feeHistory.Reward[1][0])
			slow.Add(&slow, feeHistory.Reward[2][0])
			slow.Div(&slow, big.NewInt(3))

			var average big.Int
			average.Add(feeHistory.Reward[0][1], feeHistory.Reward[1][1])
			average.Add(&average, feeHistory.Reward[2][1])
			average.Div(&average, big.NewInt(3))

			var fast big.Int
			fast.Add(feeHistory.Reward[0][2], feeHistory.Reward[1][2])
			fast.Add(&fast, feeHistory.Reward[2][2])
			fast.Div(&fast, big.NewInt(3))

			// Currently, slow/fast are not used. we use average value
			maxPriorityFeePerGasEstimate = &average
			// log.Printf("maxPriorityFeePerGasEstimate = %v", maxPriorityFeePerGasEstimate.String())

			pendingBlock, err := client.BlockByNumber(context.Background(), nil)
			checkErr(err)
			maxFeePerGasEstimate = maxFeePerGasEstimate.Add(pendingBlock.BaseFee(), maxPriorityFeePerGasEstimate)
			// log.Printf("maxFeePerGasEstimate = %v", maxFeePerGasEstimate.String())
		}

		var maxPriorityFeePerGas *big.Int
		if globalOptMaxPriorityFeePerGas == "" {
			// Use estimate value
			maxPriorityFeePerGas = maxPriorityFeePerGasEstimate
		} else {
			// Use the value set by the user
			maxPriorityFeePerGasDecimal, _ := decimal.NewFromString(globalOptMaxPriorityFeePerGas)
			// convert from gwei to wei
			maxPriorityFeePerGas = maxPriorityFeePerGasDecimal.Mul(decimal.RequireFromString("1000000000")).BigInt()
		}

		var maxFeePerGas *big.Int
		if globalOptMaxFeePerGas == "" {
			// Use estimate value
			maxFeePerGas = maxFeePerGasEstimate
		} else {
			// Use the value set by the user
			maxFeePerGasDecimal, _ := decimal.NewFromString(globalOptMaxFeePerGas)
			// convert from gwei to wei
			maxFeePerGas = maxFeePerGasDecimal.Mul(decimal.RequireFromString("1000000000")).BigInt()
		}

		tx = types.NewTx(&types.DynamicFeeTx{
			Nonce:     nonce,
			To:        toAddress, // nil means contract creation
			Value:     amount,
			Gas:       gasLimit,
			GasTipCap: maxPriorityFeePerGas,
			GasFeeCap: maxFeePerGas,
			Data:      data,
		})
	} else {
		tx = types.NewTx(&types.LegacyTx{
			Nonce:    nonce,
			To:       toAddress, // nil means contract creation
			Value:    amount,
			Gas:      gasLimit,
			GasPrice: gasPrice,
			Data:     data,
		})
	}

	chainID, err := client.NetworkID(context.Background())
	if err != nil {
		return "", fmt.Errorf("NetworkID fail: %w", err)
	}

	signedTx, err := types.SignTx(tx, types.NewLondonSigner(chainID), privateKey)
	if err != nil {
		return "", fmt.Errorf("SignTx fail: %w", err)
	}

	if globalOptShowRawTx {
		rawTx, _ := GenRawTx(signedTx)
		log.Printf("raw tx = %v", rawTx)
	}

	if globalOptShowEstimateGas {
		// EstimateGas
		msg := ethereum.CallMsg{
			From:     fromAddress,
			To:       toAddress,
			Gas:      gasLimit,
			GasPrice: gasPrice,
			Value:    amount,
			Data:     data,
		}
		gas, err := client.EstimateGas(context.Background(), msg)
		if err != nil {
			return "", fmt.Errorf("EstimateGas fail: %w", err)
		}
		log.Printf("estimate gas = %v", gas)
	}

	if globalOptDryRun {
		// return tx directly, do not broadcast it
		return signedTx.Hash().String(), nil
	}

	rpcReturnTx, err := SendRawTransaction(rpcClient, signedTx)
	if err != nil {
		return "", fmt.Errorf("SendRawTransaction fail: %w", err)
	}

	if signedTx.Hash() != *rpcReturnTx {
		log.Printf("warning: tx not same. the computed tx is %v, but rpc eth_sendRawTransaction return tx %v, use the later", signedTx.Hash(), rpcReturnTx)
	}

	if transferNotCheck {
		return rpcReturnTx.String(), nil
	}

	rp, err := getTxReceipt(client, *rpcReturnTx, 0)
	if err != nil {
		return "", fmt.Errorf("getTxReceipt fail: %w", err)
	}

	if !globalOptTerseOutput {
		// show tx explorer url only when globalOptNodeUrl in map nodeUrlMap
		for k, v := range nodeUrlMap {
			if v == globalOptNodeUrl {
				log.Printf(nodeTxExplorerUrlMap[k] + rpcReturnTx.String())
				break
			}
		}
	}

	if rp.Status != types.ReceiptStatusSuccessful {
		return "", fmt.Errorf("tx %v minted, but status is failed, please check it in block explorer", rpcReturnTx.String())
	}

	if toAddress == nil {
		log.Printf("the new contract deployed at %v", crypto.CreateAddress(fromAddress, nonce))
	}

	return rpcReturnTx.String(), nil
}

// Call invokes the (constant) contract method.
func Call(client *ethclient.Client, toAddress common.Address, data []byte) ([]byte, error) {
	opts := new(bind.CallOpts)
	msg := ethereum.CallMsg{From: opts.From, To: &toAddress, Data: data}
	ctx := context.TODO()
	return client.CallContract(ctx, msg, nil)
}

// getRecoveryId gets ecdsa recover id (0 or 1) from v.
func getRecoveryId(v *big.Int) int {
	// Note: can be simplified by checking parity (i.e. odd-even)
	var recoveryId int
	if v.Int64() == 0 || v.Int64() == 1 { // v in eip2718
		recoveryId = int(v.Int64())
	} else if v.Int64() == 27 || v.Int64() == 28 { // v before eip155
		recoveryId = int(v.Int64()) - 27
	} else { // v in eip155
		// derive chainId
		var chainId = int((v.Int64() - 35) / 2)
		// derive recoveryId
		recoveryId = int(v.Int64()) - 35 - 2*chainId
	}
	return recoveryId
}

// buildECDSASignature builds a 65-byte compact ECDSA signature (containing the recovery id as the last element)
func buildECDSASignature(v, r, s *big.Int) []byte {
	var recoveryId = getRecoveryId(v)
	// println("recoveryId", recoveryId)

	var rBytes = make([]byte, 32, 32)
	var sBytes = make([]byte, 32, 32)
	copy(rBytes[32-len(r.Bytes()):], r.Bytes())
	copy(sBytes[32-len(s.Bytes()):], s.Bytes())

	var rsBytes = append(rBytes, sBytes...)
	return append(rsBytes, byte(recoveryId))
}

// RecoverPubkey recover public key, returns 65 bytes uncompressed public key
func RecoverPubkey(v, r, s *big.Int, msg []byte) ([]byte, error) {
	signature := buildECDSASignature(v, r, s)

	// recover public key from msg (hash of data) and ECDSA signature
	// crypto.Ecrecover msg: 32 bytes hash
	// crypto.Ecrecover signature: 65-byte compact ECDSA signature
	// crypto.Ecrecover return 65 bytes uncompressed public key
	return crypto.Ecrecover(msg, signature)
}

// getFuncSig recover function signature from 4 bytes hash
// For example:
//   param: "0x8c905368"
//   return: ["NotEnoughFunds(uint256,uint256)"]
//
// This function uses openchain API
// $ curl -X 'GET' 'https://api.openchain.xyz/signature-database/v1/lookup?function=0x8c905368&filter=true'
// {"ok":true,"result":{"event":{},"function":{"0x8c905368":[{"name":"NotEnoughFunds(uint256,uint256)","filtered":false}]}}}
// See https://openchain.xyz/signatures
func GetFuncSig(funcHash string) ([]string, error) {
	var url = fmt.Sprintf("https://api.openchain.xyz/signature-database/v1/lookup?function=%s&filter=true", funcHash)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	type funcSig struct {
		Name     string `json:"name"`
		Filtered bool   `json:"filtered"`
	}
	type respMsg struct {
		Ok     bool `json:"ok"`
		Result struct {
			Function map[string][]funcSig `json:"function"`
		} `json:"result"`
	}
	var data respMsg
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}

	var rc []string
	for _, data := range data.Result.Function[funcHash] {
		rc = append(rc, data.Name)
	}

	return rc, nil
}
