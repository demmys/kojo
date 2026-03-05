import { agentApi } from "../../lib/agentApi";

interface AgentAvatarProps {
  agentId: string;
  name: string;
  size?: "sm" | "md" | "lg";
  className?: string;
}

const sizes = {
  sm: "w-8 h-8",
  md: "w-10 h-10",
  lg: "w-16 h-16",
};

export function AgentAvatar({
  agentId,
  name,
  size = "md",
  className = "",
}: AgentAvatarProps) {
  return (
    <img
      src={agentApi.avatarUrl(agentId)}
      alt={name}
      className={`${sizes[size]} rounded-full object-cover bg-neutral-800 ${className}`}
      onError={(e) => {
        // Hide broken image, show nothing
        (e.target as HTMLImageElement).style.display = "none";
      }}
    />
  );
}
