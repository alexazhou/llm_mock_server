// Package transport 承载 HTTP 传输层：Write、Flush、Hijack、取消与连接登记。
// A 阶段只建立包边界，具体实现在 C 阶段（输出与传输）落地。
package transport
