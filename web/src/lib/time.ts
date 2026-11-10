// 接口返回的时间（RFC 3339）的小函数。不依赖 React，方便单独测试（time.test.ts）。

/**
 * 排序用：新的在前。按解析出来的时间比，不按字符串比：小数秒的位数不固定（Go 会去掉末尾的 0，
 * 整秒时连小数点都没有），时区写法也可能不一样，字符串的先后和时间的先后对不上。
 */
export function newestFirst(a: string, b: string): number {
  return Date.parse(b) - Date.parse(a);
}
