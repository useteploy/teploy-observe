export interface AIConfig { provider: string; endpoint: string; model: string; has_key: boolean; api_key?: string }
export async function readAIConfig(response: Response): Promise<AIConfig> {
  const data = await response.json();
  if (!response.ok) throw new Error(data?.detail || data?.error || data?.title || `API ${response.status}`);
  if (!data || typeof data.provider !== "string" || typeof data.endpoint !== "string" || typeof data.model !== "string" || typeof data.has_key !== "boolean") throw new Error("Invalid AI configuration response");
  return data;
}
