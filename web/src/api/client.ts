import { QueryClient } from "@tanstack/react-query";
import createClient from "openapi-fetch";
import type { components, paths } from "./schema";

// schema.d.ts 是从后端的接口文档自动生成的（make gen），不要手改。
export type Meeting = components["schemas"]["Meeting"];
export type MeetingListItem = components["schemas"]["MeetingListItem"];
export type SearchHit = components["schemas"]["SearchHit"];
export type User = components["schemas"]["User"];
export type Segment = components["schemas"]["Segment"];
export type Minutes = components["schemas"]["Minutes"];
export type MinutesVersion = components["schemas"]["MinutesVersion"];
export type Template = components["schemas"]["Template"];
export type LLMSettings = components["schemas"]["LLMSettings"];
export type Status = components["schemas"]["StatusOutputBody"];
export type Member = components["schemas"]["Member"];
export type Invite = components["schemas"]["Invite"];
export type LinkInfo = components["schemas"]["LinkInfoOutputBody"];
export type AskAnswer = components["schemas"]["AskOutputBody"];
export type Citation = components["schemas"]["Citation"];
export type Device = components["schemas"]["Device"];

export const api = createClient<paths>({ baseUrl: "", credentials: "same-origin" });

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: { retry: false, refetchOnWindowFocus: false, staleTime: 5_000 },
  },
});

/** 接口返回的错误。message 是后端写好的中文说明，可以直接给用户看。 */
export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

type Problem = { detail?: string; errors?: { message?: string; location?: string }[] | null };

function describe(status: number, problem: Problem | undefined): string {
  const first = problem?.errors?.[0];
  if (first?.message) {
    // 参数校验错误：detail 只是笼统的一句，具体原因在 errors 里
    return first.location ? `${first.location}：${first.message}` : first.message;
  }
  if (problem?.detail) return problem.detail;
  if (status >= 500) return "服务器出错了，请稍后再试";
  return `请求失败（${status}）`;
}

/**
 * 把 openapi-fetch 的返回值变成「成功就给数据，失败就抛 ApiError」。
 */
export async function unwrap<T>(
  call: Promise<{ data?: T; error?: unknown; response: Response }>,
): Promise<T> {
  let result: Awaited<typeof call>;
  try {
    result = await call;
  } catch {
    throw new ApiError(0, "连不上服务器，请检查网络");
  }
  const { data, error, response } = result;
  if (response.ok) return data as T;
  // 401 是登录失效，403 可能是刚被降级：都重新读一次登录状态，界面会跟着变（回到登录页，或者收起管理员才有的入口）
  if (response.status === 401 || response.status === 403) void queryClient.invalidateQueries({ queryKey: ["status"] });
  throw new ApiError(response.status, describe(response.status, error as Problem | undefined));
}

export function errorMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
