// sherpa-onnx C API 的 cgo 封装：PCM(16k/mono/int16) → 192 维单位声纹向量。
//
// 依赖 third_party/sherpa-onnx（由 fetch_deps.sh 下载头文件与动态库）。
// 注意：sherpa-onnx 返回的是**未归一化**向量，这里补 L2 归一化 —— 缺这一步会让
// "余弦相似度"退化成尺度相关的点积（PoC 阶段实测踩过：相似度数值跑到 3~185）。
package main

/*
#cgo CFLAGS: -I${SRCDIR}/third_party/sherpa-onnx/include
#cgo LDFLAGS: -L${SRCDIR}/third_party/sherpa-onnx/lib -lsherpa-onnx-c-api -lonnxruntime -Wl,-rpath,${SRCDIR}/third_party/sherpa-onnx/lib
#include <stdlib.h>
#include "sherpa-onnx/c-api/c-api.h"
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

// Embedder 声纹提取器。并发安全：串行推理（模型推理多线程并发无收益，
// 对齐 Python sidecar 的 infer_lock 行为）。
type Embedder struct {
	mu    sync.Mutex
	ext   *C.SherpaOnnxSpeakerEmbeddingExtractor
	dim   int
	model string
}

// NewEmbedder 加载声纹模型（CAM++ ONNX）。
func NewEmbedder(modelPath string, threads int) (*Embedder, error) {
	if threads <= 0 {
		threads = 2
	}
	cmodel := C.CString(modelPath)
	cprovider := C.CString("cpu")
	defer C.free(unsafe.Pointer(cmodel))
	defer C.free(unsafe.Pointer(cprovider))

	cfg := C.SherpaOnnxSpeakerEmbeddingExtractorConfig{
		model:       cmodel,
		num_threads: C.int32_t(threads),
		debug:       C.int32_t(0),
		provider:    cprovider,
	}
	ext := C.SherpaOnnxCreateSpeakerEmbeddingExtractor(&cfg)
	if ext == nil {
		return nil, fmt.Errorf("加载声纹模型失败: %s", modelPath)
	}
	dim := int(C.SherpaOnnxSpeakerEmbeddingExtractorDim(ext))
	return &Embedder{ext: ext, dim: dim, model: modelPath}, nil
}

// Dim 嵌入维度（CAM++ 为 192）。
func (e *Embedder) Dim() int { return e.dim }

// Model 模型路径（供 /health 展示）。
func (e *Embedder) Model() string { return e.model }

// Close 释放模型。
func (e *Embedder) Close() {
	if e.ext != nil {
		C.SherpaOnnxDestroySpeakerEmbeddingExtractor(e.ext)
		e.ext = nil
	}
}

// Embed 提取声纹：PCM(16k/mono/int16 LE) → L2 归一化后的单位向量。
func (e *Embedder) Embed(pcm []byte) ([]float32, error) {
	n := len(pcm) / 2
	if n == 0 {
		return nil, fmt.Errorf("空音频")
	}
	samples := make([]float32, n)
	for i := 0; i < n; i++ {
		v := int16(uint16(pcm[i*2]) | uint16(pcm[i*2+1])<<8)
		samples[i] = float32(v) / 32768.0
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	stream := C.SherpaOnnxSpeakerEmbeddingExtractorCreateStream(e.ext)
	if stream == nil {
		return nil, fmt.Errorf("创建音频流失败")
	}
	defer C.SherpaOnnxDestroyOnlineStream(stream)

	C.SherpaOnnxOnlineStreamAcceptWaveform(stream, C.int32_t(16000),
		(*C.float)(unsafe.Pointer(&samples[0])), C.int32_t(len(samples)))
	C.SherpaOnnxOnlineStreamInputFinished(stream)
	if C.SherpaOnnxSpeakerEmbeddingExtractorIsReady(e.ext, stream) == 0 {
		return nil, fmt.Errorf("音频不足以计算声纹")
	}
	v := C.SherpaOnnxSpeakerEmbeddingExtractorComputeEmbedding(e.ext, stream)
	if v == nil {
		return nil, fmt.Errorf("计算声纹失败")
	}
	emb := make([]float32, e.dim)
	copy(emb, unsafe.Slice((*float32)(unsafe.Pointer(v)), e.dim))
	C.SherpaOnnxSpeakerEmbeddingExtractorDestroyEmbedding(v)

	normalize(emb)
	return emb, nil
}
