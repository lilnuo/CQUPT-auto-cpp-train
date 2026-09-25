package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

// ============================================================================
// 任务密码的加密存储
//
// 为什么必须加密：server 接收的是学生真实的学号密码。把明文写进数据库之后，
// 只要库被 dump、备份泄漏、或者某个只读账号被盗，就等于把所有人的密码
// 送了出去——而且很多人会在别处复用同一个密码。
//
// 方案：AES-256-GCM。
//   AES   = Advanced Encryption Standard，高级加密标准，对称分组密码
//   GCM   = Galois/Counter Mode，伽罗瓦/计数器模式，一种**认证加密**模式
//
// 选 GCM 而不是更常见的 CBC 的理由：GCM 同时提供机密性和完整性校验。
// CBC 只管加密不管防篡改——攻击者可以在不知道密钥的情况下翻转密文比特，
// 解密出来就是一串被悄悄改过的明文，而你毫无察觉。GCM 会在解密时校验
// 认证标签，密文被动过就直接报错。
//
// 密文格式：nonce || ciphertext || tag
//   nonce = number used once，一次性随机数。GCM 要求**同一密钥下 nonce 绝不重复**，
//   重复会让攻击者通过异或还原出明文。12 字节随机 nonce 在实际中可以认为不会撞，
//   所以每次加密都新生成一个，并把它拼在密文前面，解密时再切出来。
// ============================================================================

// nonceSize 是 GCM 推荐的标准 nonce 长度（12 字节）。
const nonceSize = 12

// deriveKey 把任意长度的口令串成 32 字节密钥。
//
// AES-256 要求密钥恰好 32 字节。用 SHA-256 做一次摘要，就能把"用户在 .env 里
// 随手写的长口令"变成固定长度的合法密钥。
//
// 注意这是**权宜之计而非最佳实践**：正经做法应该用 HKDF 之类的密钥派生函数
// 做带盐的慢速派生（抗暴力破解）。这里选择 SHA-256 是因为密钥来自环境变量、
// 已经是高熵的机器配置而不是人脑记的密码。如果你的密钥是人挑的短口令，
// 请换成 bcrypt/scrypt/argon2 这类慢哈希。
func deriveKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// EncryptPassword 加密任务密码，返回可直接存入 VARBINARY 列的字节。
func EncryptPassword(secret, plaintext string) ([]byte, error) {
	if secret == "" {
		return nil, errors.New("加密密钥为空")
	}
	block, err := aes.NewCipher(deriveKey(secret))
	if err != nil {
		return nil, fmt.Errorf("构造 AES 分组密码失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("构造 GCM 失败: %w", err)
	}

	nonce := make([]byte, nonceSize)
	// 从 crypto/rand 取随机数。绝不能用 math/rand：
	// 它的输出是可预测的，拿它做 nonce 等于把 nonce 的随机性白送掉。
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("生成 nonce 失败: %w", err)
	}

	// Seal 返回 ciphertext||tag，我们把 nonce 拼在最前面
	return gcm.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// DecryptPassword 解出任务密码。
func DecryptPassword(secret string, blob []byte) (string, error) {
	if secret == "" {
		return "", errors.New("加密密钥为空")
	}
	if len(blob) < nonceSize+1 {
		return "", fmt.Errorf("密文长度不足（%d 字节），数据可能已损坏", len(blob))
	}
	block, err := aes.NewCipher(deriveKey(secret))
	if err != nil {
		return "", fmt.Errorf("构造 AES 分组密码失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("构造 GCM 失败: %w", err)
	}

	nonce, ciphertext := blob[:nonceSize], blob[nonceSize:]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// 这里既可能是密钥换了，也可能是密文被篡改——GCM 分不出来，也不必分，
		// 对调用方来说处理方式相同：这条任务的密码解不开了。
		return "", errors.New("解密失败：密钥不匹配或密文已被篡改")
	}
	return string(plain), nil
}
