import { createContext, useContext } from "react";

/** What the console knows about the Master from GET /api/worker/node-info. */
export type ServerInfo = {
  // The zone in which the Master reads task time filters, e.g. "UTC".
  timeZone?: string;
};

export const ServerInfoContext = createContext<ServerInfo>({});

export const useServerInfo = () => useContext(ServerInfoContext);
