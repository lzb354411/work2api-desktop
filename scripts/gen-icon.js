// gen-icon.js 生成应用图标（纯 Node 标准库：手写 PNG/ICO 编码）。
// 产出：build/appicon.png(512) + build/appicon.ico + build/windows/icon.ico
// 设计：深色圆角方块 + 双向箭头（代理/转发语义）。
const zlib = require('zlib')
const fs = require('fs')
const path = require('path')

function crc32(buf) {
  let c, crc = 0xffffffff
  for (let n = 0; n < buf.length; n++) {
    c = (crc ^ buf[n]) & 0xff
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
    crc = (crc >>> 8) ^ c
  }
  return (crc ^ 0xffffffff) >>> 0
}

function encodePNG(size, drawFn) {
  const S = size
  const px = new Uint8Array(S * S * 4)
  const put = (x, y, [r, g, b, a]) => {
    if (x < 0 || y < 0 || x >= S || y >= S) return
    const i = (y * S + x) * 4
    // alpha 混合
    if (a >= 255) { px[i] = r; px[i+1] = g; px[i+2] = b; px[i+3] = 255 }
    else {
      const oa = px[i+3] / 255, na = a / 255, ma = na + oa * (1 - na)
      px[i]   = Math.round((r * na + px[i]   * oa * (1 - na)) / ma)
      px[i+1] = Math.round((g * na + px[i+1] * oa * (1 - na)) / ma)
      px[i+2] = Math.round((b * na + px[i+2] * oa * (1 - na)) / ma)
      px[i+3] = Math.round(ma * 255)
    }
  }
  drawFn(put, S)

  // raw scanlines
  const raw = Buffer.alloc(S * (S * 4 + 1))
  for (let y = 0; y < S; y++) {
    raw[y * (S * 4 + 1)] = 0
    px.subarray(y * S * 4, (y + 1) * S * 4).forEach((v, i) => {
      raw[y * (S * 4 + 1) + 1 + i] = v
    })
  }
  const idat = zlib.deflateSync(raw, { level: 9 })

  const chunk = (type, data) => {
    const len = Buffer.alloc(4); len.writeUInt32BE(data.length)
    const td = Buffer.concat([Buffer.from(type, 'ascii'), data])
    const crc = Buffer.alloc(4); crc.writeUInt32BE(crc32(td))
    return Buffer.concat([len, td, crc])
  }
  const ihdr = Buffer.alloc(13)
  ihdr.writeUInt32BE(S, 0); ihdr.writeUInt32BE(S, 4)
  ihdr[8] = 8; ihdr[9] = 6; ihdr[10] = 0; ihdr[11] = 0; ihdr[12] = 0
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', idat),
    chunk('IEND', Buffer.alloc(0)),
  ])
}

// PNG-in-ICO 封装
function pngToIco(pngBuf, size) {
  const header = Buffer.alloc(6)
  header.writeUInt16LE(0, 0); header.writeUInt16LE(1, 2); header.writeUInt16LE(1, 4)
  const entry = Buffer.alloc(16)
  entry[0] = size >= 256 ? 0 : size
  entry[1] = size >= 256 ? 0 : size
  entry[2] = 0; entry[3] = 0
  entry.writeUInt16LE(1, 4)   // planes
  entry.writeUInt16LE(32, 6)  // bpp
  entry.writeUInt32LE(pngBuf.length, 8)
  entry.writeUInt32LE(22, 12) // offset = 6+16
  return Buffer.concat([header, entry, pngBuf])
}

// ---------- 绘制 ----------
const BG = [30, 34, 46, 255]      // #1e222e
const ACCENT = [79, 140, 255, 255] // #4f8cff
const WHITE = [235, 239, 248, 255]

function drawIcon(put, S) {
  const k = S / 64 // 归一化系数
  // 圆角方块背景
  const r = 13 * k
  for (let y = 0; y < S; y++) {
    for (let x = 0; x < S; x++) {
      const cx = Math.max(r, Math.min(x, S - 1 - r))
      const cy = Math.max(r, Math.min(y, S - 1 - r))
      const dx = x - cx, dy = y - cy
      if (dx * dx + dy * dy <= r * r) put(x, y, BG)
    }
  }
  // 上箭头（向右）：矩形杆 + 三角头
  const barY = Math.round(20 * k), barH = Math.max(2, Math.round(4 * k))
  const x0 = Math.round(13 * k), x1 = Math.round(44 * k)
  for (let y = barY; y < barY + barH; y++)
    for (let x = x0; x <= x1; x++) put(x, y, ACCENT)
  const tipX = Math.round(50 * k), cY = barY + Math.floor(barH / 2)
  const half = Math.round(6 * k)
  for (let dy = -half; dy <= half; dy++) {
    const w = Math.round((1 - Math.abs(dy) / (half + 1)) * (tipX - x1))
    for (let dx = 0; dx <= w; dx++) put(tipX - dx, cY + dy, ACCENT)
  }
  // 下箭头（向左）
  const barY2 = Math.round(40 * k)
  for (let y = barY2; y < barY2 + barH; y++)
    for (let x = x0; x <= x1; x++) put(x, y, WHITE)
  const tipX2 = Math.round(14 * k), cY2 = barY2 + Math.floor(barH / 2)
  for (let dy = -half; dy <= half; dy++) {
    const w = Math.round((1 - Math.abs(dy) / (half + 1)) * (x0 + 6 * k - tipX2))
    for (let dx = 0; dx <= w; dx++) put(tipX2 + dx, cY2 + dy, WHITE)
  }
}

// ---------- 输出 ----------
const out = (p, buf) => {
  fs.mkdirSync(path.dirname(p), { recursive: true })
  fs.writeFileSync(p, buf)
  console.log('written', p, buf.length, 'bytes')
}

const root = path.join(__dirname, '..')
const png512 = encodePNG(512, drawIcon)
const png64 = encodePNG(64, drawIcon)
out(path.join(root, 'build', 'appicon.png'), png512)
out(path.join(root, 'build', 'appicon.ico'), pngToIco(png64, 64))
out(path.join(root, 'build', 'windows', 'icon.ico'), pngToIco(encodePNG(256, drawIcon), 256))
console.log('done')
