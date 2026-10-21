import { redirect } from "next/navigation";

export default function Home() {
  redirect("/function/tasks");
  return <>準備中</>;
}
