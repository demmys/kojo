import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Routes, Route, Navigate } from "react-router";
import { Dashboard } from "./components/Dashboard";
import { SessionPage } from "./components/SessionPage";
import { NewSession } from "./components/NewSession";
import { FileBrowser } from "./components/FileBrowser";
import { AgentChat } from "./components/agent/AgentChat";
import { AgentCreate } from "./components/agent/AgentCreate";
import { AgentSettings } from "./components/agent/AgentSettings";
import { AgentCredentials } from "./components/agent/AgentCredentials";
import { GroupDMChat } from "./components/groupdm/GroupDMChat";
import { GlobalSettings } from "./components/GlobalSettings";
import "./index.css";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter>
      <Routes>
        <Route path="/" element={<Dashboard />} />
        <Route path="/session/:id" element={<SessionPage />} />
        <Route path="/session/:id/terminal" element={<SessionPage />} />
        <Route path="/session/:id/files" element={<SessionPage />} />
        <Route path="/session/:id/git" element={<SessionPage />} />
        <Route path="/session/:id/attachments" element={<SessionPage />} />
        <Route path="/new" element={<NewSession />} />
        <Route path="/files" element={<FileBrowser />} />
        <Route path="/agents" element={<Navigate to="/" replace />} />
        <Route path="/agents/new" element={<AgentCreate />} />
        <Route path="/agents/:id" element={<AgentChat />} />
        <Route path="/agents/:id/settings" element={<AgentSettings />} />
        <Route path="/agents/:id/credentials" element={<AgentCredentials />} />
        <Route path="/groupdms/:id" element={<GroupDMChat />} />
        <Route path="/settings" element={<GlobalSettings />} />
      </Routes>
    </BrowserRouter>
  </StrictMode>,
);
