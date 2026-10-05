package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Canonicalize 把 QueryState 规范化为**字典序稳定**的字符串，用于：
//   - 查询哈希（缓存键）
//   - 幂等性断言（闸门 G1：相同 QueryState ⇒ 相同结果）
//
// 实现要点：Go 的 encoding/json 对 map 的键默认按字典序输出，
// 但数组顺序**原样保留**。因此这里对「语义上无序」的数组（filters / dims 各维度）
// 显式排序，保证 {a,b} 与 {b,a} 得到同一哈希。
func Canonicalize(s QueryState) (string, error) {
	c := s // 值拷贝，避免污染入参

	// filters：按 (field, op, value 序列化) 排序
	sort.SliceStable(c.Filters, func(i, j int) bool {
		ki := c.Filters[i].Field + "\x00" + c.Filters[i].Op + "\x00" + mustJSON(c.Filters[i].Value)
		kj := c.Filters[j].Field + "\x00" + c.Filters[j].Op + "\x00" + mustJSON(c.Filters[j].Value)
		return ki < kj
	})
	// dims 各维度数组排序
	sort.Strings(c.Dims.Brand)
	sort.Strings(c.Dims.Domain)
	sort.Strings(c.Dims.ChannelCode)
	sort.Strings(c.Dims.StoreKey)
	sort.Strings(c.Dims.Category)
	sort.Strings(c.Dims.ProductStatus)

	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("canonicalize: marshal: %w", err)
	}
	return string(b), nil
}

// Hash 返回 QueryState 的规范化哈希（sha256 前 32 位十六进制）。
func Hash(s QueryState) (string, error) {
	canon, err := Canonicalize(s)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(sum[:16]), nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("!err:%v", err)
	}
	return string(b)
}
