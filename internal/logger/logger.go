package logger

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Creates and returns a new Zap logger instance
// Its user responsibility to call Sync() on the returned logger
func NewZapLogger(module, filepath string) (*zap.Logger, error) {
	file, err := createFileIfNotExists(filepath)
	if err != nil {
		return nil, err
	}

	writer := zapcore.AddSync(file)
	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.TimeKey = "time"
	encoderCfg.MessageKey = "message"
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		writer,
		zap.DebugLevel,
	)

	fiedsOption := zap.Fields(zap.String("module", module))
	stachTraceOption := zap.AddStacktrace(zap.ErrorLevel)
	callerOption := zap.AddCaller()

	return zap.New(core, callerOption, stachTraceOption, fiedsOption), nil
}

func createFileIfNotExists(path string) (*os.File, error) {
	_, err := os.Stat(path)
	if err != nil && os.IsNotExist(err) {
		return os.OpenFile(
			path,
			os.O_CREATE|os.O_EXCL|os.O_WRONLY,
			0644,
		)
	}

	return os.OpenFile(
		path,
		os.O_APPEND|os.O_WRONLY,
		0644,
	)
}
