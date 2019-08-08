package anomaly

import (
	"fmt"
	"github.com/sensorbee/jubatus/internal/pluginutil"
	"github.com/ugorji/go/codec"
	"gopkg.in/sensorbee/sensorbee.v0/bql/udf"
	"gopkg.in/sensorbee/sensorbee.v0/core"
	"gopkg.in/sensorbee/sensorbee.v0/data"
	"io"
	"reflect"
	"strings"
)

type anomalyMsgpack struct {
	_struct       struct{} `codec:",toarray"`
	FormatVersion uint8
	Algorithm     string
}

type lightLOFState struct {
	lightLOF           *LightLOF
	featureVectorField string
}

var _ core.SavableSharedState = &lightLOFState{}

type lightLOFStateMsgpack struct {
	_struct            struct{} `codec:",toarray"`
	FeatureVectorField string
}

type LightLOFStateCreator struct {
}

var _ udf.UDSLoader = &LightLOFStateCreator{}

func (c *LightLOFStateCreator) CreateState(ctx *core.Context, params data.Map) (core.SharedState, error) {
	fv, err := pluginutil.ExtractParamAsStringWithDefault(params, "feature_vector_field", "feature_vector")
	if err != nil {
		return nil, err
	}

	nnAlgoName, err := pluginutil.ExtractParamAsString(params, "nearest_neighbor_algorithm")
	if err != nil {
		return nil, err
	}

	var nnAlgo NNAlgorithm
	switch strings.ToLower(nnAlgoName) {
	case "lsh":
		nnAlgo = LSH
	case "minhash":
		nnAlgo = Minhash
	case "euclid_lsh":
		nnAlgo = EuclidLSH
	default:
		return nil, fmt.Errorf("invalid nearest_neighbor_algorithm: %s", nnAlgoName)
	}

	hashNum, err := pluginutil.ExtractParamAsInt(params, "hash_num")
	if err != nil {
		return nil, err
	}
	nnNum, err := pluginutil.ExtractParamAsInt(params, "nearest_neighbor_num")
	if err != nil {
		return nil, err
	}
	rnnNum, err := pluginutil.ExtractParamAsInt(params, "reverse_nearest_neighbor_num")
	if err != nil {
		return nil, err
	}

	unlearn, err := pluginutil.ExtractParamAsStringWithDefault(params, "unlearner", "no")
	if err != nil {
		return nil, err
	}
	var maxSize int
	var seed int64
	switch unlearn {
	case "no":
		maxSize = 0
	case "random":
		m, err := pluginutil.ExtractParamAsInt(params, "max_size")
		if err != nil {
			return nil, err
		}
		maxSize = int(m)

		seed, err = pluginutil.ExtractParamAsIntWithDefault(params, "seed", 0)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("invalid unlearner: %v", unlearn)
	}

	// TODO: check hashNum, nnNum, rnnNum <= INT_MAX
	llof, err := NewLightLOF(nnAlgo, int(hashNum), int(nnNum), int(rnnNum), maxSize, seed)
	if err != nil {
		return nil, err
	}
	return &lightLOFState{
		lightLOF:           llof,
		featureVectorField: fv,
	}, nil
}

var (
	anomalyMsgpackHandle = &codec.MsgpackHandle{}
)

func init() {
	anomalyMsgpackHandle.MapType = reflect.TypeOf(map[string]interface{}{})
}

func (c *LightLOFStateCreator) LoadState(ctx *core.Context, r io.Reader, params data.Map) (core.SharedState, error) {
	var d anomalyMsgpack
	dec := codec.NewDecoder(r, anomalyMsgpackHandle)
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	if d.Algorithm != "light_lof" {
		return nil, fmt.Errorf("unsupported anomaly detection algorithm: %v", d.Algorithm)
	}

	switch d.FormatVersion {
	case 1:
		return loadLightLOFStateFormatV1(ctx, r)
	default:
		return nil, fmt.Errorf("unsupported format version of LightLOFState container: %v", d.FormatVersion)
	}
}

func loadLightLOFStateFormatV1(ctx *core.Context, r io.Reader) (core.SharedState, error) {
	s := &lightLOFState{}

	var d lightLOFStateMsgpack
	dec := codec.NewDecoder(r, anomalyMsgpackHandle)
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	s.featureVectorField = d.FeatureVectorField

	llof, err := LoadLightLOF(r)
	if err != nil {
		return nil, err
	}
	s.lightLOF = llof
	return s, nil
}

func (*lightLOFState) Terminate(ctx *core.Context) error {
	return nil
}

func (l *lightLOFState) Write(ctx *core.Context, t *core.Tuple) error {
	vfv, ok := t.Data[l.featureVectorField]
	if !ok {
		return fmt.Errorf("%s field is missing", l.featureVectorField)
	}
	fv, err := data.AsMap(vfv)
	if err != nil {
		return fmt.Errorf("%s value is not a map: %v", l.featureVectorField, err)
	}

	err = l.lightLOF.AddWithoutCalcScore(FeatureVector(fv))
	return err
}

const (
	anomalyFormatVersion = 1
)

func (l *lightLOFState) Save(ctx *core.Context, w io.Writer, params data.Map) error {
	enc := codec.NewEncoder(w, anomalyMsgpackHandle)
	if err := enc.Encode(&anomalyMsgpack{
		FormatVersion: anomalyFormatVersion,
		Algorithm:     "light_lof",
	}); err != nil {
		return err
	}

	if err := enc.Encode(&lightLOFStateMsgpack{
		FeatureVectorField: l.featureVectorField,
	}); err != nil {
		return err
	}
	return l.lightLOF.Save(w)
}

func AddAndGetScore(ctx *core.Context, stateName string, featureVector data.Map) (float32, error) {
	l, err := lookupLightLOFState(ctx, stateName)
	if err != nil {
		return 0, err
	}

	score, err := l.lightLOF.Add(FeatureVector(featureVector))
	if err != nil {
		return 0, err
	}

	return score, nil
}

func CalcScore(ctx *core.Context, stateName string, featureVector data.Map) (float32, error) {
	l, err := lookupLightLOFState(ctx, stateName)
	if err != nil {
		return 0, err
	}

	score, err := l.lightLOF.CalcScore(FeatureVector(featureVector))
	if err != nil {
		return 0, err
	}

	return score, nil
}

func lookupLightLOFState(ctx *core.Context, stateName string) (*lightLOFState, error) {
	st, err := ctx.SharedStates.Get(stateName)
	if err != nil {
		return nil, err
	}

	if l, ok := st.(*lightLOFState); ok {
		return l, nil
	}
	return nil, fmt.Errorf("state '%v' cannot be converted to lightLOFState", stateName)
}
